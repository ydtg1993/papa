package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/core"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

var ErrTaskOwnershipConflict = errors.New("existing task belongs to another site, stage or account pool")

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	if task == nil {
		return err
	}
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// validateTask 提交前的最小校验（stage/url/site 必填、阶段已登记）。
//
// 抽出来给 SubmitTask 与 submitRecoveredTask 共用：两条路对入参的要求必须一致 ——
// 恢复/轮询队列从库里重建的 Task 也可能因为历史脏行缺少字段，不能因为省一次查库就少校验一道。
func (e *Engine) validateTask(task *Task) error {
	if task == nil || task.Stage == "" || task.URL == "" || task.Site == "" {
		return fmt.Errorf("task stage or url or site is empty: %+v", task)
	}
	if task.Delay < 0 {
		return fmt.Errorf("task delay must not be negative")
	}
	if info := e.stages[stageKeyOf(task)]; info == nil {
		return fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/FETCHER_WRITING_GUIDE.md 7.1）", task.Stage, task.Site)
	}
	resourceID := task.ResourceID
	if resourceID == "" {
		resourceID = task.IdempotencyKey
	}
	if _, err := core.NormalizeTaskIdentity(task.Site, task.Stage, resourceID, task.URL); err != nil {
		return err
	}

	return nil
}

// submitIfActive 若任务尚未到终态（success/failed）则延迟投递或入队，否则静默跳过。
//
// 没有返回值：这条路上不存在"失败" —— 入队、延迟投递、寄放到溢出表等回灌，三种都是
// "任务已经有人管了"（见 submitToPool 末尾）。
func (e *Engine) submitIfActive(task *Task, record models.CrawlerTask) {
	if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
		return
	}
	// 延迟投递：到点才入队，避免 worker 空等浪费并发位
	if record.NotBefore != nil {
		task.NotBefore = *record.NotBefore
		task.Delay = time.Duration(record.DelayMS) * time.Millisecond
	}
	if at := task.NotBefore; !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, record)
		return
	}
	e.submitToPool(task, record)
}

// submitToPool 将任务提交到对应阶段工作池，并更新数据库状态。
//
// **没有返回值**：这条路上不存在"投递失败" —— 三类结局（落库写不进去 · 队满 · 池已停机/被回收）
// 统统归到同一个动作：任务留在溢出表等回灌（见函数末尾）。所以调用方不需要、也不该处理错误，
// 由类型把这件事说出来；原先签名带 error 而三条出口全是 nil，那几处 err 处理是死代码。
func (e *Engine) submitToPool(task *Task, record models.CrawlerTask) {
	if record.NotBefore != nil {
		task.NotBefore = *record.NotBefore
	}
	if e.deferUntilDue(task) {
		return
	}
	// 先落库再入队：「已入队 ⇒ 行里是 pending」必须是不变量。
	// worker 取到任务时会以 status=待处理 为条件认领（claimTask），
	// 如果这里先入队后落库，中间那个窗口里取到任务的 worker（repeatable 重跑时
	// 行里还是"成功"）会被误判成"已被运营改动"而跳过执行。
	// 只写这个函数负责的那几列，不再整行 Save(record)：record 是更早 SELECT 出来的快照，
	// 而**延迟投递**那条会把它存进 delayHeap、到点才走到这里（Delay 多长就旧多久）。
	// 整行写回会把运营在这期间改的列（repeatable / urgent / error…）静默盖回去 ——
	// 尤其是 repeatable：它现在可由后台「开/停轮询」随时改，被盖回去就是"控制面被数据面吃掉"。
	// 写不进去就**不入队**：上面那句「已入队 ⇒ 行里是 pending」全靠这一条 UPDATE 撑着。
	// 报错还照旧入队的话，worker 取到它会因认领不到（claimTask 要的正是 pending）而跳过执行 ——
	// 任务在队列里待过、谁都没跑过，库里那行还停在旧状态上，两头都看不出发生过什么。
	//
	// 处理方式与投递失败一致：寄放到溢出表，回灌时重读行、重跑这一句，权当一次重试。
	// **只看 Error，不看 RowsAffected**：MySQL 的 UPDATE 默认返回「真正被改动的行数」，
	// 行已经是 pending、meta/site 也没变时它照样是 0，拿它当失败会平白寄放一堆正常任务。
	// 所以状态守卫不能靠 RowsAffected 判（0 分不出"已经是 pending"与"已被认领"），只能写在 WHERE 里：
	// 行若在 SELECT 之后被 worker 认领成了 processing，`status = pending` 匹配不到、不把它翻回 pending，
	// 正在跑的旧 worker 才不会被回灌 / 延迟投递重置掉（见 drainSpilled 跳过 processing 那一跳）。
	// 匹配不到还照旧入队：入队的那份副本 claimTask（同样以 pending 为条件）认领不到、直接跳过，
	// 不会造成二次执行。
	if err := e.submissions().preparePending(task, record.ID); err != nil {
		e.loggerSet.Engine.Errorf("update task %d before enqueue failed, task kept in spill list for retry: %s", task.ID, err.Error())
		e.spillTask(task)
		return
	}
	task.Repeat = record.Repeat
	if task.Repeatable && task.ID != 0 && !task.repeatCounted {
		if task.Repeat == 0 {
			task.Repeat = 1
		}
		// 首轮初始化是持久化幂等的；对象标记仅避免同一副本反复初始化。
		// 已有 pending 轮次（包括重建对象/启动恢复）不得消耗新的轮询次数。
		task.repeatCounted = true
	}
	e.afterTaskPending(task)
}

// submitPendingTask 行**已经**是 pending 时只做投递，不再写回状态。
//
// 给"刚从批次里捞出整行、刚做完条件重置"的恢复/轮询队列用：它们手上的行状态已经落定，
// 再执行一次 submitToPool 的 UPDATE 只会刷新 updated_at（一条无信息量的写）。
func (e *Engine) submitPendingTask(task *Task) {
	e.afterTaskPending(task)
}

// afterTaskPending 任务行已处于（或已写回）pending 之后的收尾：叫醒轮询队列 + 投递。
func (e *Engine) afterTaskPending(task *Task) {
	if e.deferUntilDue(task) {
		return
	}
	if task.RepeatInterval > 0 {
		// 新播种了一条带周期的轮询任务：叫醒轮询队列重算扫描节拍，别让它等当前那次 sleep。
		e.wakeRepeatQueue()
	}

	// 投递失败的两条路都归到同一个动作：**任务留在溢出表里等回灌**（行保持 pending）。
	//
	//   - 队列达 75% 高水位（ErrQueueFull）：这是真·队列压力，计进 spilledCount。
	//   - 其它投递错误：实测只有一种来源 —— 拿到的池刚被空闲回收 / 已停机，
	//     Submit 报 "worker pool already stopped"。它不是容量问题，所以不进那次计数。
	//
	// **这里一度是把行标成 failed 的**。换掉它有两个理由：
	//   - 池被 reapIdle 回收不是这条任务的错：它判空（队列空 + 无在途）与我们 Submit 之间有窗口 ——
	//     调用方已经从表里拿到池、还没投进去，池就先被停了。拿内部竞态给业务判 failed 是让任务背锅；
	//   - failed **不在启动恢复的集合里**（recoverqueue 只收 pending/processing）：标了就只剩错误队列那一跳
	//     （默认 4h 一轮、max_retry 3 代）能捞回来 —— 中间这几个小时后台显示的是「失败」，业务看到的是一次
	//     根本没发生的失败；错误队列关着或代数用尽，就真的回不来了。
	// 改成重新寄放后，"不标 failed 会永远停在 pending、没有队列会再捞它"这个担心也不成立：
	// 溢出表就是干这个的 —— drain 每 drain_interval（默认 2s）重投一次，进程真退了还有启动恢复兜底。
	if err := e.submitToStage(task); err != nil {
		if errors.Is(err, workerpool.ErrQueueFull) {
			e.spilledCount.Add(1)
			e.spillTask(task)
			return
		}
		e.loggerSet.Engine.Warnf("submit to stage failed, task kept in spill list for retry: stage=%q id=%d err=%v",
			stageKeyOf(task), task.ID, err)
		e.spillTask(task)
	}
}

// submitRecoveredTask 把恢复队列/轮询队列刚条件重置成 pending 的任务投回队列。
//
// 与 SubmitTask 的差别只有一个：调用方手里已经握着那一行（刚从批次查出来、刚做完条件更新），
// 不需要再按去重键查一次库，也不需要再写一次 pending。Repeatable 任务仍走 submitToPool ——
// 它负责 `repeat` +1（轮询次数），那是这条路径的既有语义，不能因为省写就丢。
func (e *Engine) submitRecoveredTask(task *Task) error {
	if err := e.validateTask(task); err != nil {
		return err
	}
	e.dedupCache.Add(task.Unique())
	if task.Repeatable {
		e.submitIfActive(task, models.CrawlerTask{ID: uint(task.ID), Status: models.TaskStatusPending, Repeat: task.Repeat})
		return nil
	}
	e.submitPendingTask(task)
	return nil
}

// claimTask 把任务从「待处理」认领为「处理中」，成功才允许执行。
// 条件更新而非先读再写：运营在它被 worker 取走之前标了失败（或删了行）时，
// 这里拿不到行，任务就不再执行 —— 这正是「标失败」对排队中任务的拦截力。
// 顺带把 urgent 归零：加急是「排队位置」的概念，跑过一次就完成使命，
// 不清的话失败重投会让加急任务越积越多、快车道被老任务长期占住。
// 返回 (是否认领成功, 错误)：出错不能当作执行许可；worker 保留责任并重试认领。
func (e *Engine) claimTask(task *Task) (bool, error) {
	return e.claimTaskContext(context.Background(), task)
}

func (e *Engine) claimTaskContext(ctx context.Context, task *Task) (bool, error) {
	if task.FlowID != "" {
		return e.claimFlowTask(ctx, task)
	}
	if task.ID == 0 {
		return true, nil // 未落库的任务（理论上不该出现）：没有行可认领，直接执行
	}
	claimed, row, err := e.submissions().claim(ctx, uint(task.ID))
	if err != nil {
		return false, err
	}
	if row != nil && row.Status == models.TaskStatusPending && row.NotBefore != nil {
		task.NotBefore = *row.NotBefore
		at := time.Now().Add(e.drainInterval())
		if task.NotBefore.After(at) {
			at = task.NotBefore
		}
		e.enqueueDelayed(at, task, *row)
	}
	return claimed, nil
}

// submitToStage 所有投递路径的收口：取活池（没有就懒建），按加急/常规路由。
// 阶段未注册 → ErrStageNotRegistered（后台据此翻 409）。
func (e *Engine) submitToStage(task *Task) error {
	// 暂停中：**在入队点收口**，不接新活。
	//
	// 返回 ErrQueueFull 而不是自己往溢出表里塞：五个调用点已经把这个错误处理得正好
	// （`submitToPool` 计数 spilledCount + 溢出并返回 nil、错误队列、后台重投/加急、延迟投递）——
	// 自己再来一遍会漏掉那个计数（`queue_spilled` 指标失真），还让那几处的处理变成死代码。
	//
	// 放在建池**之前**：暂停期间连池子都不该建（没有 worker、没有统计器），
	// 恢复时第一个任务进来才懒建 —— 这也让"暂停期间什么都不会发生"这条性质干净利落。
	if e.isPaused(pauseScope{Site: task.Site, Stage: task.Stage}) {
		return fmt.Errorf("%w: 站点 %q 的阶段 %q 已暂停，任务先寄放（恢复后自动灌回）",
			workerpool.ErrQueueFull, task.Site, task.Stage)
	}
	pool := e.getOrCreatePool(stageKeyOf(task))
	if pool == nil {
		// 拿不到池有两种原因，给运维的结论完全不同，别混成一句"阶段未注册"：
		// 引擎在停机（活池登记表置了停机位，从此不再建池）—— 任务先寄放，行保持 pending，
		// 下次启动的启动恢复重新入队。用 ErrQueueFull 包住是**故意的**：五个投递调用点对
		// ErrQueueFull 的处理正是"寄放、不判失败、不建池"，与停机要的语义逐字相同
		//（暂停那条路也是这么复用的，见上面 isPaused 那段）。
		if e.pools.IsStopped() {
			return fmt.Errorf("%w: 引擎已停机，站点 %q 的阶段 %q 先寄放（行保持 pending，下次启动的启动恢复会重新入队）",
				workerpool.ErrQueueFull, task.Site, task.Stage)
		}
		return fmt.Errorf("%w: %s", ErrStageNotRegistered, task.Stage)
	}
	if task.Urgent {
		return pool.SubmitUrgent(task)
	}
	return pool.Submit(task)
}

func (e *Engine) submissions() submissionStore {
	var references taskReferenceGuard
	if m := e.GetAccountManager(); m != nil && m.Pools() != nil {
		references = m.Pools()
	}
	var logger submissionLogger
	if e.loggerSet != nil {
		logger = e.loggerSet.Engine
	}
	return submissionStore{db: e.db, ctx: e.ctx, dedupCache: e.dedupCache, references: references, logger: logger}
}

// SubmitTask validates and persists one task before accepting delivery responsibility.
func (e *Engine) SubmitTask(task *Task) error {
	if task != nil && task.FlowID != "" {
		return ErrFlowInvalid
	}
	if err := e.validateTask(task); err != nil {
		return e.logSubmitError(task, err)
	}
	if task.ID == 0 {
		task.AccountPool = e.resolveTaskPool(task)
	}
	plan, err := e.submissions().prepare(task)
	e.deliverSubmission(plan)
	if plan.logFailure {
		return e.logSubmitError(task, err)
	}
	return err
}

// SubmitTasks retains successful rows even when other members of the batch fail.
// Duplicate inputs receive the first task's persisted identity and repeat state.
func (e *Engine) SubmitTasks(tasks []*Task) error {
	for _, task := range tasks {
		if task != nil && task.FlowID != "" {
			return ErrFlowInvalid
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	for _, task := range tasks {
		if err := e.validateTask(task); err != nil {
			return err
		}
		if task.ID == 0 {
			task.AccountPool = e.resolveTaskPool(task)
		}
	}
	_, err := e.submissions().prepareBatch(tasks, func(delivery submissionDelivery) {
		e.deliverSubmission(submissionPlan{deliveries: []submissionDelivery{delivery}})
	})
	return err
}

// Delivery accepts responsibility even if admission also reported partial errors.
func (e *Engine) deliverSubmission(plan submissionPlan) {
	for _, delivery := range plan.deliveries {
		task := delivery.task
		if delivery.unread {
			if at := task.deliverAt(); delivery.batch && !at.IsZero() && at.After(time.Now()) {
				e.enqueueDelayed(at, task, models.CrawlerTask{ID: uint(task.ID)})
			} else {
				e.spillTask(task)
			}
			continue
		}
		if !delivery.batch {
			e.submitIfActive(task, delivery.record)
			continue
		}
		record := delivery.record
		if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
			continue
		}
		if record.NotBefore != nil {
			task.NotBefore = *record.NotBefore
		}
		if at := task.NotBefore; !at.IsZero() && at.After(time.Now()) {
			e.enqueueDelayed(at, task, record)
			continue
		}
		e.submitToPool(task, record)
	}
}
