package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
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
		Site:    task.Site,
		TaskID:  task.ID,
		URL:     task.URL,
		Retry:   task.Retry,
		Kind:    ErrorKind(err),
		Message: err.Error(),
	}
	e.loggerSet.Engine.WithFields(logrus.Fields{
		"stage":   te.Stage,
		"site":    te.Site,
		"task_id": te.TaskID,
		"url":     te.URL,
		"retry":   te.Retry,
		"kind":    te.Kind,
	}).Errorf("task failed: %s", te.Message)

	// 熔断计数。走到这里就是**终态失败**（重试耗尽、或不可重试），正好是熔断要数的那个量 ——
	// 按 attempt 数会把一个烂 URL 记 3 次，阈值会被噪声灌满。见 breaker.RecordFailure。
	// **记在该阶段所属站点的闸门上**：多站时站点 A 的失败不该把站点 B 一起闸住。
	e.breakerFor(e.siteOf(task.Stage)).RecordFailure(task.Stage)

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
			Site:    st.Site,
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
	// 阶段的存在性只有一个来源：**注册进来的 e.stages**（配置里不再声明阶段，见 App.RegisterSites）。
	// 于是"配置声明了、代码没实现"这一类不同源的问题在结构上不存在了；未注册的阶段在提交时
	// 由 submitToPool 报 ErrStageNotRegistered。
	names := e.stageNames()

	// 第零趟之二：依赖接线校验。声明了要用下载器却没接线，就在**启动这一刻**炸掉 ——
	// 不然那句话要等第一条任务跑到那一步才出现，失败的是任务、不是启动（见 deps.go）。
	for _, stage := range names {
		e.checkStageDeps(stage, e.stages[stage].fetcher)
	}

	// 第一趟：把**所有**阶段的工作池建起来并启动。
	for _, stage := range names {
		stageInfo := e.stages[stage]
		cfg := stageInfo.config
		pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
		// 闸门**按站点**绑：该阶段的池子由它所属站点的闸门管；未归属站点的阶段落在默认 scope、
		// 用 crawler.breaker 那把。单站项目 = 一个隐式 scope = 一把闸门，与之前完全一致。
		// 必须在 Start 之前：worker 直接读这个字段，Start 之后再设就是数据竞争。
		pool.SetGate(e.breakerFor(stageInfo.config.Site))
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

	// 第二趟：池子都建好、也都启动了，这时才跑各阶段的入口任务。
	//
	// **必须两趟**：入口回调最自然的写法就是"投一批起始任务"，而它可以投给**任意**阶段。
	// 一趟遍历（边建池边跑回调）时，后面的池子还没建，`submitTo` 就是 nil 解引用 panic
	//（Go 的 map 遍历顺序随机，所以表现为"有时崩、有时不崩"）。
	//
	// 引擎在这里**不做策略判断**：有回调就跑（要不要接入口回调，由声明层决定 —— 见
	// App.RegisterSites 的 AutoStart）。
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
	// 启动页面归档的保留期清理（归档未开启时不启动）
	e.startArchiveCleanup()
	// HTTP 服务开启时低频采样三队列积压（COUNT 查询，不进监控页请求路径）
	if e.cfg.Server.Enabled {
		e.startQueueSampler()
	}
}

// runAttempt 执行一次 FetchHandler，并负责这一次尝试的步骤追踪与页面归档：
// 挂记录器 → 跑 handler → 记下结局 → 落盘（归档）/落库（trace）。
//
// 两件事都放在 defer 里，所以 panic 展开时也会执行 —— 闭包这一层的 defer 先于 workerpool
// 的 recover 跑，panic 之前已上报的步骤因此不会丢；panic 路径上 setResult 没被调用，
// trace 按失败处理（data 保留）、归档也按失败落盘，正好是排查现场要看的。
//
// 这里 recover 一次**只是为了拿"是不是 panic"这个信号**（panic 时命名返回值 err 还是 nil），
// 拿到之后原样抛回去：workerpool 那头的 debug.Stack() 与 failed 计数才是既有行为，
// 吞掉 panic 或者重新造一个都会丢掉 handler 的栈帧。
func (e *Engine) runAttempt(ctx context.Context, fetcher Fetcher, task *Task, attempt int) (err error) {
	tr := e.newTrace(task, attempt)
	task.Trace = tr
	defer tr.finish() // finish 幂等，正常返回与 panic 展开都走这里

	// 归档缓冲（未开归档时是 nil，所有方法都 nil 安全）。这个 defer 注册在 tr.finish 之后，
	// 于是**先**执行（LIFO）——"这次归档了哪些文件"因此能作为一条步骤写进还没落库的 trace。
	ar := e.newArchiveBuffer(task, attempt)
	defer func() {
		r := recover()
		if files := ar.finish(r != nil || err != nil); len(files) > 0 {
			// OA 追踪抽屉里"失败"与"那一页"的连接点：光有文件、不知道属于哪条任务等于没留。
			// 注意这条步骤的 data 也受 trace 的既有策略约束（只有失败的尝试才保留 data）——
			// `always` 模式下成功的尝试里，步骤名还在但文件名会被剥掉；那种情况按
			// `{dir}/{stage}/task-{id}-try-{retry}-*` 在目录里找同一次尝试的文件即可。
			tr.Step(archiveTraceStep, map[string]any{"files": files})
		}
		// 失败原因写进 trace：handler 返回错误、或直接 panic，都在这里落一条带分类与消息的
		// 步骤。放在归档步骤之后 —— 现场在前、"为什么死"在后，读起来就是这次尝试的顺序。
		switch {
		case r != nil:
			tr.Fail(failureTraceStep, fmt.Errorf("panic: %v", r), nil)
			panic(r) // 原样抛回：workerpool 那头的栈与计数是既有行为
		case err != nil:
			tr.Fail(failureTraceStep, err, nil)
		}
	}()
	// 缓冲挂在 ctx 上：FetchHTML 只拿得到 ctx，这是它知道"这一页属于哪次尝试"的唯一途径
	ctx = withArchive(ctx, ar)
	// 站点请求头也挂在 ctx 上：这个站的任务抓任何页面都自动带上它的 UA/Cookie/Referer，
	// handler 不用写代码；要对单次请求再改，用 `papa.WithHeaders(ctx, …)` 叠加（它只会覆盖给到的键）。
	if h := e.siteHeaders(task.Site); len(h) > 0 {
		ctx = core.WithHeaders(ctx, h)
	}

	// 加急记在 trace 的第一步，事后还能看出"这条曾经加急跑过"：
	// claimTask 认领时会把 urgent 列归零（加急是「排队位置」的概念，跑过一次即完成使命），
	// 那张表上就再也看不出它加急过了。只记第一次尝试 —— 同一次执行里的后续重试不是新的加急。
	if attempt == 0 && task.Urgent {
		tr.Step(traceUrgentStep, nil)
	}

	err = fetcher.FetchHandler(ctx, task, e)
	tr.setResult(err)
	return err
}
