package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// validateTask 提交前的最小校验（stage/url/site 必填、阶段已登记）。
//
// 抽出来给 SubmitTask 与 submitRecoveredTask 共用：两条路对入参的要求必须一致 ——
// 恢复/轮询队列从库里重建的 Task 也可能因为历史脏行缺少字段，不能因为省一次查库就少校验一道。
func (e *Engine) validateTask(task *Task) error {
	if task.Stage == "" || task.URL == "" || task.Site == "" {
		return fmt.Errorf("task stage or url or site is empty: %+v", task)
	}
	if info := e.stages[stageKeyOf(task)]; info == nil {
		return fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/FETCHER_WRITING_GUIDE.md 7.1）", task.Stage, task.Site)
	}
	return nil
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task) error {
	if err := e.validateTask(task); err != nil {
		return e.logSubmitError(task, err)
	}

	key := task.Unique()
	var record models.CrawlerTask

	// 两阶段去重：先查内存缓存，miss 再查 DB（唯一索引 idx_site_stage_url_hash 兜底）
	if e.dedupCache.Get(key) {
		if !task.Repeatable {
			return nil // 去重命中：视为成功，无需重复处理
		}
		// 已入库的轮询任务：查找记录防止重复录入
		if err := e.findTaskRecord(task, &record); err != nil {
			return e.logSubmitError(task, fmt.Errorf("load repeat task record: %w", err))
		}
		task.ID = int(record.ID)
		e.submitIfActive(task, record)
		return nil
	}

	err := e.findTaskRecord(task, &record)
	if err == nil {
		// DB 命中：复用已有记录（重新加入内存缓存）
		e.dedupCache.Add(key)
		wasNew := task.ID == 0
		task.ID = int(record.ID)
		// 仅「新任务」去重跳过；已带 ID 的重提交（如 RecoverJob）需继续入队
		if !task.Repeatable && wasNew {
			return nil
		}
		e.submitIfActive(task, record)
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return e.logSubmitError(task, fmt.Errorf("query task dedup: %w", err))
	}

	// 全新任务：提交到 pool 前先插入数据库
	if ierr := task.Insert(e.db); ierr != nil {
		// 并发下撞唯一索引：另一 goroutine 已入库并将入队，此处回填 ID 后跳过，避免重复入队
		if rerr := e.findTaskRecordFallback(task, &record); rerr != nil {
			return e.logSubmitError(task, fmt.Errorf("insert crawler task to db failed: %w", ierr))
		}
		e.dedupCache.Add(key)
		task.ID = int(record.ID)
		return nil
	}
	e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record)
	e.dedupCache.Add(key)
	e.submitIfActive(task, record)
	return nil
}

// findTaskRecord 按去重键查找已存在的任务记录；未找到返回 gorm.ErrRecordNotFound。
//
// 两条分支都**按唯一索引查**（查得到的前提是"查的键与索引的键一致"，否则插冲突后回查会空手而归、
// 把并发冲突报成插入失败）：
//   - 自定义幂等键 → `idempotency_hash`（唯一索引 idx_idempotency_hash；键原文 varchar(1000) 建不了索引）
//   - 没有自定义键 → `url_hash + stage + site`（唯一索引 idx_site_stage_url_hash）
//
// 调用方只用 ID 与 Status（入队前判终态），所以只投影这两列：历史行的 error/meta 可能很宽，
// 为了去重把整行读出来纯属浪费。
func (e *Engine) findTaskRecord(task *Task, record *models.CrawlerTask) error {
	return e.dedupScope(task).Select("id", "status").First(record).Error
}

// dedupScope 给出"查已存在行"的条件（抽出来是为了能用 DryRun 把 SQL 形状钉住 —— 假库不解析 WHERE）。
func (e *Engine) dedupScope(task *Task) *gorm.DB {
	q := e.db.Model(&models.CrawlerTask{})
	if h := models.IdempotencyKeyHash(task.IdempotencyKey); h != "" {
		return q.Where("idempotency_hash = ?", h)
	}
	return q.Where("url_hash = ? AND stage = ? AND site = ?", models.UrlHash(task.URL), task.Stage, task.Site)
}

// findTaskRecordFallback 插入撞了唯一索引之后**无条件**按两条唯一索引回查。
//
// 为什么不能复用 findTaskRecord：它只走"这次提交用的那条键"。冲突完全可能来自**另一条**索引 ——
// 最典型的是幂等键文本变过（老行还存着旧文本，新代码算出的哈希查不到它），而 (url_hash, stage, site)
// 那条索引照样拦下这次插入。这时若只按哈希回查就会空手而归，把"复用已有行"误报成插入失败。
// 所以先按哈希（若有自定义键），再按 (url_hash, stage, site)。
func (e *Engine) findTaskRecordFallback(task *Task, record *models.CrawlerTask) error {
	if h := models.IdempotencyKeyHash(task.IdempotencyKey); h != "" {
		if err := e.db.Model(&models.CrawlerTask{}).Select("id").Where("idempotency_hash = ?", h).First(record).Error; err == nil {
			return nil
		}
	}
	return e.db.Model(&models.CrawlerTask{}).Select("id").
		Where("url_hash = ? AND stage = ? AND site = ?", models.UrlHash(task.URL), task.Stage, task.Site).
		First(record).Error
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
	if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
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
	if res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", record.ID, models.TaskStatusPending).
		Updates(submitUpdates(task)); res.Error != nil {
		e.loggerSet.Engine.Errorf("update task %d before enqueue failed, task kept in spill list for retry: %s",
			task.ID, res.Error.Error())
		e.spillTask(task)
		return
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
		e.submitIfActive(task, models.CrawlerTask{ID: uint(task.ID), Status: models.TaskStatusPending})
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
// 返回 (是否认领成功, 错误)：DB 抖动不当成"认领失败"，由调用方记日志后继续执行，
// 免得一次抖动让任务永远没人跑。
func (e *Engine) claimTask(task *Task) (bool, error) {
	if task.ID == 0 {
		return true, nil // 未落库的任务（理论上不该出现）：没有行可认领，直接执行
	}
	res := claimScope(e.db, uint(task.ID)).Updates(map[string]any{
		"status": models.TaskStatusProcessing,
		"urgent": false,
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// submitTo 按任务的加急标记选队列：加急走快车道，其余走常规队列。
// 池子本身不认识优先级（Tasker 接口只有 Unique），路由决定留在这里。
// submitUpdates 一次提交要写回行里的列 —— **只写这个函数负责的那几列**（见 submitToPool 的注释）。
//
// 抽出来是为了能在**不连库**的前提下把拼出来的 SQL 渲染出来验（engine/sqlshape_test.go）：
// `repeat` 是 MySQL 保留字，裸写进表达式会被当成 REPEAT() 函数调用、整条 UPDATE 报 1064 ——
// 这个坑真踩过（v3.1.0 起，只有真库跑得出来，假库测试看不见）。
func submitUpdates(task *Task) map[string]any {
	updates := map[string]any{"status": models.TaskStatusPending}
	if task.Repeatable && task.ID != 0 {
		// 轮询代数交给 SQL 自增：读出来 +1 再写回，并发下会丢别人的增量。
		// 列名必须加引号（`repeat` 是保留字，见函数注释）。
		updates["repeat"] = gorm.Expr("`repeat` + 1")
	}
	// 这次提交带了 Meta 就刷新这一列（带空 Meta 的提交不动它）。
	// 行是这条任务的记录，而重投路径全靠它还原身份 —— 不刷新的话库里会一直留着上一次的身份，
	// 下一次重投就把任务带回旧的业务行上（SubmitTask/SubmitTasks 的 Meta 只是内存里的，
	// 谁也看不出来它和库里的不一致）。
	if len(task.Meta) > 0 {
		updates["meta"] = metaToJSON(task.Meta)
	}
	// 站点同理：行是这条任务的记录，重投路径全靠它认出"这条属于哪个站"
	if task.Site != "" {
		updates["site"] = task.Site
	}
	return updates
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
		if e.pools.isStopped() {
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

// SubmitTasks 批量提交任务：一次性批量入库（减少 DB 往返），再逐个入队。
func (e *Engine) SubmitTasks(tasks []*Task) error {
	if len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if t.Stage == "" || t.URL == "" || t.Site == "" {
			return e.logSubmitError(t, fmt.Errorf("task stage or url or site is empty: %+v", t))
		}
		if info := e.stages[stageKeyOf(t)]; info == nil {
			return e.logSubmitError(t, fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/FETCHER_WRITING_GUIDE.md 7.1）", t.Stage, t.Site))
		}
	}

	// 两阶段去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	for _, t := range tasks {
		key := t.Unique()
		if e.dedupCache.Get(key) {
			if !t.Repeatable {
				continue // 去重命中：跳过
			}
			var record models.CrawlerTask
			if err := e.findTaskRecord(t, &record); err != nil {
				return e.logSubmitError(t, fmt.Errorf("load repeat task record: %w", err))
			}
			t.ID = int(record.ID)
		} else {
			var record models.CrawlerTask
			err := e.findTaskRecord(t, &record)
			switch {
			case err == nil:
				// DB 命中：复用已有记录
				e.dedupCache.Add(key)
				t.ID = int(record.ID)
				if !t.Repeatable {
					continue
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				if t.ID == 0 {
					toInsert = append(toInsert, t)
				}
			default:
				return e.logSubmitError(t, fmt.Errorf("query task dedup: %w", err))
			}
		}
		toProcess = append(toProcess, t)
	}

	// 批量入库（单条多行 INSERT），失败退到逐条
	var conflicted map[string]struct{}
	if len(toInsert) > 0 {
		var err error
		conflicted, err = e.insertTasks(toInsert)
		if err != nil {
			err = fmt.Errorf("insert crawler tasks: %w", err)
			e.loggerSet.Engine.Errorf("submit tasks failed: %v", err)
			return err
		}
	}

	// 逐个入队（含延迟投递）。这里没有"投递失败"要收集：投不进去的任务会进溢出表等回灌，
	// 真正会失败的只有前面那道批量插入（它已经返回了）。
	for _, t := range toProcess {
		// 并发撞车的那几条：库里那行已经被另一路入库并入队了，别再入一次（与 SubmitTask 同一处理）
		if _, hit := conflicted[t.Unique()]; hit {
			e.dedupCache.Add(t.Unique())
			continue
		}
		e.dedupCache.Add(t.Unique())
		var record models.CrawlerTask
		e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record)
		if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
			continue
		}
		if at := t.deliverAt(); !at.IsZero() && at.After(time.Now()) {
			e.enqueueDelayed(at, t, record)
			continue
		}
		e.submitToPool(t, record)
	}
	return nil
}

// insertTasks 批量入库并回填 ID，返回其中「撞了唯一索引、复用了库里已有行」的那些任务的去重键。
//
// 快路径就是一条多行 INSERT —— SubmitTasks 本来就是冲着减少 DB 往返来的。
//
// 但**一行冲突不该让整批（可能上万条）一条都进不去**，而且还直接返回错误、连队列都没进，
// 业务看到的是一整批任务凭空消失。所以批量失败就退到逐条插。
//
// 这里不去分辨"批量失败是不是因为重复键"：逐条那条路自己会分辨 —— 插不进去就按唯一索引
// `idx_site_stage_url_hash` 回查一下，查得到说明确实是并发撞车（别的 goroutine / 进程在这两步之间
// 把同样的 site|stage|url 写进去了），回填它的 ID、记进返回的集合；查不到说明插入是真的失败了，
// 原样把插入的那个错报出去（它比"没查到"更有信息量）。
// 这么写也就不依赖 gorm 的 TranslateError（默认没开）去识别 MySQL 的 1062。
func (e *Engine) insertTasks(tasks []*Task) (map[string]struct{}, error) {
	records := make([]models.CrawlerTask, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, t.toModel())
	}

	if err := e.db.Create(&records).Error; err == nil {
		for i, t := range tasks {
			t.ID = int(records[i].ID)
		}
		return nil, nil
	}

	e.loggerSet.Engine.Warnf("batch insert failed, falling back to per-row insert (%d tasks)", len(tasks))
	conflicted := make(map[string]struct{})
	for _, t := range tasks {
		rec := t.toModel()
		ierr := e.db.Create(&rec).Error
		if ierr == nil {
			t.ID = int(rec.ID)
			continue
		}
		// 按两条唯一索引回查（哈希优先，其次 (url_hash, stage)）—— 冲突落在哪条都接得住，
		// 而只查一条时"键文本变过"那种情况会把它报成插入失败。
		var existed models.CrawlerTask
		if serr := e.findTaskRecordFallback(t, &existed); serr != nil {
			return nil, fmt.Errorf("insert task %q: %w", t.URL, ierr)
		}
		t.ID = int(existed.ID)
		conflicted[t.Unique()] = struct{}{}
	}
	return conflicted, nil
}
