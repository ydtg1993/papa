package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/internal/track"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
)

// AddNotifier 注册告警通知器；任务最终失败时触发 Notify。
func (e *Engine) AddNotifier(n Notifier) {
	if n == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifiers = append(e.notifiers, n)
}

func (e *Engine) getNotifiers() []Notifier {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Notifier, len(e.notifiers))
	copy(out, e.notifiers)
	return out
}

// notifyFailure 结构化记录失败日志，并触发告警通知器。
func (e *Engine) notifyFailure(ctx context.Context, task *Task, err error) {
	te := TaskError{
		Stage:   task.Stage,
		TaskID:  task.ID,
		URL:     task.URL,
		Retry:   task.Retry,
		Kind:    ErrorKind(err),
		Message: err.Error(),
	}
	e.loggerSet.Engine.WithFields(logrus.Fields{
		"stage":   te.Stage,
		"task_id": te.TaskID,
		"url":     te.URL,
		"retry":   te.Retry,
		"kind":    te.Kind,
	}).Errorf("task failed: %s", te.Message)

	// 熔断计数。走到这里就是**终态失败**（重试耗尽、或不可重试），正好是熔断要数的那个量 ——
	// 按 attempt 数会把一个烂 URL 记 3 次，阈值会被噪声灌满。见 breaker.RecordFailure。
	e.breaker.RecordFailure(task.Stage)

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	level := AlertError
	if Retryable(err) {
		level = AlertWarn
	}
	event := AlertEvent{Level: level, TaskError: te}
	for _, n := range notifiers {
		if nerr := n.Notify(ctx, event); nerr != nil {
			e.loggerSet.Engine.Errorf("notify alert failed: %s", nerr.Error())
		}
	}
}

// notifyBreakerTrip 熔断触发时的回调：打醒目日志 + 发一条 AlertCritical 告警。
//
// 走的是与任务失败**同一个** Notifier 通道，业务不用再接一套告警 ——
// 区别只在 level（critical 比 error 高一级），webhook 那边可以据此单独路由（钉钉 @全体之类）。
func (e *Engine) notifyBreakerTrip(st BreakerStatus) {
	e.loggerSet.Engine.Errorf(
		"熔断触发：%s（阶段 %s，%s 窗口内 %d 次终态失败 ≥ 阈值 %d）—— 已闸住所有阶段的 worker，"+
			"处理完后调 POST /api/breaker/resume（后台 Dashboard 上也有按钮）放行",
		st.Reason, st.Stage, st.Window, st.Failures, st.Threshold)

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	event := AlertEvent{
		Level: AlertCritical,
		TaskError: TaskError{
			Stage:   st.Stage,
			Kind:    "circuit-breaker",
			Message: st.Reason,
		},
	}
	for _, n := range notifiers {
		if err := n.Notify(e.ctx, event); err != nil {
			e.loggerSet.Engine.Errorf("notify breaker trip failed: %s", err.Error())
		}
	}
}

// stageNames 按名字排序返回已注册的阶段名。
//
// 建池与跑 submitFunc 都按这个顺序（而不是 map 遍历顺序）：启动期的副作用是**看得见**的
// —— 起始任务入库的顺序决定它们的 ID，统计器注册的顺序决定后台阶段的排列。map 顺序随机，
// 每次重启都不一样，排查"为什么这次启动多了两条任务"时就成了噪声源。
func (e *Engine) stageNames() []string {
	names := make([]string, 0, len(e.stages))
	for name := range e.stages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ApplyRegisterStage 启用注册业务流程开启对应工作池
func (e *Engine) ApplyRegisterStage() {
	names := e.stageNames()

	// 第零趟：依赖接线校验。声明了要用下载器却没接线，就在**启动这一刻**炸掉 ——
	// 不然那句话要等第一条任务跑到那一步才出现，失败的是任务、不是启动（见 deps.go）。
	for _, stage := range names {
		e.checkStageDeps(stage, e.stages[stage].fetcher)
	}

	// 第一趟：把**所有**阶段的工作池建起来并启动。
	for _, stage := range names {
		stageInfo := e.stages[stage]
		cfg := stageInfo.config
		pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
		// 同一把闸门给所有阶段的池子 —— 熔断是「全任务暂停」，不是按阶段各停各的。
		// 必须在 Start 之前：worker 直接读这个字段，Start 之后再设就是数据竞争。
		pool.SetGate(e.breaker)
		e.stages[stage].workerPool = pool
		// 启动 worker pool
		pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
			// 认领：把「待处理」置为「处理中」，让运营侧能看出这条归谁管，
			// 也让三个后台动作的状态守卫成立。拿不到行就跳过执行 —— 原因不止一种：
			// 运营已经动过它（标失败/删除），或同一行的另一份副本（后台「加急」会另投一份）先认领了。
			claimed, err := e.claimTask(task)
			if err != nil {
				e.loggerSet.Engine.Errorf("claim task %d: %s", task.ID, err.Error())
			} else if !claimed {
				e.loggerSet.Engine.Warnf("task %d 已被认领或改动（不再是待处理），跳过本次执行", task.ID)
				return nil
			}
			// 重试FetchHandler
			var lastErr error
			for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
				if attempt > 0 {
					// 退避放在**下一次尝试之前**（与 m3u8 / filedown 的退避同一写法）。
					// 它不能写在上一轮的末尾：那样最后一次尝试失败之后还要再睡满一个周期才落 failed，
					// 而那时已经不会再试了 —— worker 只是抱着一个并发位白等
					//（3 次尝试、30s 退避就是 120s；实测一个注定失败的 detail 任务从认领到终态 210s）。
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(cfg.Backoff * (1 << uint(attempt-1))):
					}
					task.IncRetry(e.db)
				}
				err := e.runAttempt(ctx, stageInfo.fetcher, task, attempt)
				if err == nil {
					task.UpdateStatus(e.db, models.TaskStatusSuccess, nil)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					// 任务间隔延迟：让 worker 在两次抓取之间歇一下，别把目标站打急。
					// **必须可被 ctx 打断** —— 任务到这里已经成功、状态也已经落库，
					// 再让 worker 抱着这个并发位睡满一个 delay（模板里 catalog 是 5m），
					// 只会让 Engine.Stop（默认只等 stop_timeout=5s）报"未排空"、
					// 连带跳过 app.Run 里的关库收尾。停机时直接跳过这段休息。
					// 与上面重试退避那段同一个写法（Engine.Stop 的注释承诺的就是这个）。
					select {
					case <-ctx.Done():
					case <-time.After(cfg.Delay.Random()):
					}
					return nil
				}
				// 不可重试的错误：直接标 failed，不再空转重试
				if !Retryable(err) {
					task.UpdateStatus(e.db, models.TaskStatusFailed, err)
					e.notifyFailure(ctx, task, err)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, err)
				}
				lastErr = err
			}
			// 所有重试失败：记录错误并更新状态为 failed
			task.UpdateStatus(e.db, models.TaskStatusFailed, lastErr)
			e.notifyFailure(ctx, task, lastErr)
			if !task.Repeatable {
				e.DelActiveTask(task)
			}
			return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, lastErr)
		})
		// HTTP 服务开启时，为该阶段创建统计器并启动（数据供监控页面的阶段概览用）
		if e.cfg.Server.Enabled {
			stats := track.NewStatsQueue(pool)
			stats.Start(e.ctx)
			e.setStatsQueue(stage, stats)
			e.loggerSet.Monitor.Infof("monitor started for stage: %s", stage)
		}
	}

	// 第二趟：池子都建好、也都启动了，这时才跑各阶段的 submitFunc。
	//
	// **必须两趟**：submitFunc 最自然的写法就是"投一批起始任务"，而它可以投给**任意**阶段。
	// 一趟遍历（边建池边跑回调）时，后面的池子还没建，`submitTo` 就是 nil 解引用 panic
	// （Go 的 map 遍历顺序随机，所以表现为"有时崩、有时不崩"）。触发前提是"回调投给别的阶段"，
	// 脚手架与文档里的单阶段投自己碰不到 —— 但代价是启动期崩溃且报错为空，两趟的成本是零。
	for _, stage := range names {
		if fn := e.stages[stage].submitFunc; fn != nil {
			fn(e)
		}
	}

	// 启动高水位溢出任务的回灌协程
	e.startDrain()
	// 启动错误队列后台自动轮询（未配置 interval 则不启动，仅手动触发）
	e.startErrorQueue()
	// 启动中断恢复队列（启用时启动即恢复一次 + 定时轮询）
	e.startRecoverQueue()
	// 启动周期轮询队列（repeatable 任务的定时重跑）
	e.startRepeatQueue()
	// 启动步骤追踪的保留期清理（追踪未开启时不启动）
	e.startTraceCleanup()
	// HTTP 服务开启时低频采样三队列积压（COUNT 查询，不进监控页请求路径）
	if e.cfg.Server.Enabled {
		e.startQueueSampler()
	}
}

// runAttempt 执行一次 FetchHandler，并负责这一次尝试的步骤追踪：
// 挂记录器 → 跑 handler → 记下结局 → 落库。
//
// 落库放在 defer 里，所以 panic 展开时也会执行 —— 闭包这一层的 defer 先于 workerpool
// 的 recover 跑，panic 之前已上报的步骤因此不会丢；panic 路径上 setResult 没被调用，
// flush 按失败处理、data 保留，正好是排查现场要看的。
// 这里**不 recover**：panic 继续抛给 workerpool，它那边的 debug.Stack() 与 failed
// 计数才是既有行为，重新 panic 反而会丢掉 handler 的栈帧。
func (e *Engine) runAttempt(ctx context.Context, fetcher Fetcher, task *Task, attempt int) error {
	tr := e.newTrace(task, attempt)
	task.Trace = tr
	defer tr.finish() // finish 幂等，正常返回与 panic 展开都走这里

	// 加急记在 trace 的第一步，事后还能看出"这条曾经加急跑过"：
	// claimTask 认领时会把 urgent 列归零（加急是「排队位置」的概念，跑过一次即完成使命），
	// 那张表上就再也看不出它加急过了。只记第一次尝试 —— 同一次执行里的后续重试不是新的加急。
	if attempt == 0 && task.Urgent {
		tr.Step(traceUrgentStep, nil)
	}

	err := fetcher.FetchHandler(ctx, task, e)
	tr.setResult(err)
	return err
}
