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

	// 熔断计数 + 触发。走到这里就是**终态失败**（重试耗尽、或不可重试），正好是熔断要数的那个量 ——
	// 按 attempt 数会把一个烂 URL 记 3 次，阈值会被噪声灌满。见 breaker.RecordFailure。
	//
	// **计数在熔断器里（每站一份），"停"这个动作在暂停信号里**：达到阈值就写一条整站暂停
	//（Source=breaker，带上"哪个阶段在失败、当时窗口内多少次"的现场）。
	// **写成功才发告警** —— "一次触发只报一次"因此由 setPause 的去重天然保证：
	// 已经停着时它返回 false（可能是人工暂停在先），那就既不该覆盖人家的原因，也不该重复报警。
	// 站点没有熔断配置时 breakerFor 返回 nil，RecordFailure 是 nil 安全的（没有熔断这回事）。
	if tripped, failures := e.breakerFor(task.Site).RecordFailure(); tripped {
		if e.setPause(pauseScope{Site: task.Site}, pauseState{
			Reason:   "窗口内终态失败数达到阈值",
			Source:   PauseSourceBreaker,
			Stage:    task.Stage,
			Failures: failures,
		}) {
			e.notifyBreakerTrip(e.breakerStatusOf(task.Site))
		}
	}

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
		"熔断触发：%s（阶段 %s，%s 窗口内 %d 次终态失败 ≥ 阈值 %d）—— 已暂停整站："+
			"worker 停在取任务点，新任务不再入队（先寄放到溢出表）。"+
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

// stageKeys 按名字排序返回已注册的**阶段键**（`core.StageKey`，含站点）。
//
// 建池与跑 submitFunc 都按这个顺序（而不是 map 遍历顺序）：启动期的副作用是**看得见**的
// —— 起始任务入库的顺序决定它们的 ID，统计器注册的顺序决定后台阶段的排列。map 顺序随机，
// 每次重启都不一样，排查"为什么这次启动多了两条任务"时就成了噪声源。
// 排序结果按"站点 → 阶段名"排（键就是 `site:stage`），多站时同一个站的阶段聚在一起。
func (e *Engine) stageKeys() []string {
	names := make([]string, 0, len(e.stages))
	for name := range e.stages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// stageHandler 一个阶段 worker 的任务处理函数：认领 → 带重试地跑 handler → 落终态。
// 从 ApplyRegisterStage 的第一趟提出来：池子懒建（首次 submit 才建），建的时候绑上同一个 handler。
func (e *Engine) stageHandler(info *stageInfo) workerpool.TaskHandler[*Task] {
	cfg := info.config
	// 尝试次数兜底为 1：声明层（planStage）已经保证 > 0，但直接调 AddStage 的用法可以传 0 ——
	// 那一档的循环一次都不跑，然后拿着**还是 nil** 的 lastErr 去落 failed、进 notifyFailure
	//（那里会 err.Error()），当场空指针 panic。只兜"会崩"的这一类，不动其它字段的语义
	//（与下载器那边 max(MaxConcurrent, 1) 同一路数）。
	attempts := max(cfg.MaxAttempts, 1)
	return func(ctx context.Context, task *Task) error {
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
		for attempt := 0; attempt < attempts; attempt++ {
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
			err := e.runAttempt(ctx, info.fetcher, task, attempt)
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
	}
}

// buildPool 建一个阶段的工作池并登记：建池 + 按站点绑闸门 + 起 worker + 建监控 stats。
// 调用方须保证 key（`core.StageKey`，不是裸阶段名）已注册。
func (e *Engine) buildPool(key string, info *stageInfo) *workerpool.WorkerPool[*Task] {
	cfg := info.config
	pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
	// 闸门绑**这个池自己的 scope**（站点 + 阶段）：它问的是暂停信号，所以"整站暂停"与
	// "某个阶段暂停"由同一个适配器回答，不需要在池子上串两道闸。
	// 必须在 Start 之前：worker 直接读这个字段，Start 之后再设就是数据竞争。
	pool.SetGate(e.pauseGateFor(pauseScope{Site: cfg.Site, Stage: info.stage}))
	pool.Start(e.ctx, e.stageHandler(info))
	e.startPoolErrorForwarder(key, pool)
	// HTTP 服务开启时，为该阶段创建统计器并启动（数据供监控页面的阶段概览用）
	if e.cfg.Server.Enabled {
		stats := track.NewStatsQueue(pool)
		stats.Start(e.ctx)
		e.setStatsQueue(key, stats)
		e.loggerSet.Monitor.Infof("monitor started for stage: %s", key)
	}
	return pool
}

// getOrCreatePool 取某阶段的活池；没有就按 StageConfig 懒建一个并登记。阶段未注册返回 nil。
// 入参是**阶段键**（`core.StageKey(site, stage)`）—— 阶段名只在本站内唯一，两个站的 catalog
// 是两个池子，所以键里必须带站点。
// 每次都走 getOrCreate（而不是先 get 快路径）—— 要让 lastSubmit 在**每次投递**时刷新，
// 空闲回收靠它判断池闲置了多久。
func (e *Engine) getOrCreatePool(key string) *workerpool.WorkerPool[*Task] {
	info := e.stages[key]
	if info == nil {
		return nil
	}
	return e.pools.getOrCreate(key, func() *workerpool.WorkerPool[*Task] {
		return e.buildPool(key, info)
	})
}

// removeStageStats 撤掉某阶段的监控统计器并停掉它的 goroutine（池被回收时配套清理）。
// pool 是那份被回收的池：清 stats 与回收之间，同阶段可能已被重新提交、建了新池新 stats ——
// 只清「仍属于这份池」的 stats，别把新建的那份一起删掉。
func (e *Engine) removeStageStats(key string, pool *workerpool.WorkerPool[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if stats, ok := e.statsQueue[key]; ok && stats.WorkPool == pool {
		stats.Stop()
		delete(e.statsQueue, key)
	}
}

// startPoolErrorForwarder 起一条转发协程，把池的错误通道汇进引擎的常驻错误通道（Errors()）。
// pool.Errors() 从不 close，所以必须带停机信号 —— 池被 reap 时 close 它，否则协程泄漏到引擎结束。
func (e *Engine) startPoolErrorForwarder(key string, pool *workerpool.WorkerPool[*Task]) {
	stop := make(chan struct{})
	e.mu.Lock()
	if e.errorForwardCancel == nil {
		e.errorForwardCancel = make(map[string]chan struct{})
	}
	if old, ok := e.errorForwardCancel[key]; ok {
		close(old) // 旧池的转发协程该退了（池被重建）
	}
	e.errorForwardCancel[key] = stop
	e.mu.Unlock()

	go func() {
		for {
			select {
			case err := <-pool.Errors():
				select {
				case e.errorCh <- err:
				default: // 常驻通道满了就丢：错误通道是兜底，主日志路径已经记过
				}
			case <-stop:
				return
			case <-e.ctx.Done():
				return
			}
		}
	}()
}

// cancelPoolErrorForwarder 停掉某阶段的错误转发协程（池被 reap 时配套调用）。
func (e *Engine) cancelPoolErrorForwarder(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if stop, ok := e.errorForwardCancel[key]; ok {
		close(stop)
		delete(e.errorForwardCancel, key)
	}
}

// startIdleReaper 启动空闲池回收协程：每隔 stage_idle_timeout/2 扫一遍，回收「超过 idle 没有
// 新任务、且空着」的池。config 没配 stage_idle_timeout（<=0）就不起 —— 默认常驻，行为与之前一致。
func (e *Engine) startIdleReaper() {
	idle := e.cfg.Crawler.StageIdleTimeout
	if idle <= 0 {
		return
	}
	interval := idle / 2
	if interval < time.Second {
		interval = time.Second
	}
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				for key, pool := range e.pools.reapIdle(idle, e.cfg.Crawler.StopTimeoutOrDefault()) {
					e.harvestStageTotals(key, pool)
					e.removeStageStats(key, pool)
					e.cancelPoolErrorForwarder(key)
					e.loggerSet.Engine.Infof("reaped idle stage pool: %s", key)
				}
			}
		}
	}()
}

// ApplyRegisterStage 启用注册业务流程开启对应工作池
func (e *Engine) ApplyRegisterStage() {
	// 阶段的存在性只有一个来源：**注册进来的 e.stages**（配置里不再声明阶段，见 App.RegisterSites）。
	// 于是"配置声明了、代码没实现"这一类不同源的问题在结构上不存在了；未注册的阶段在提交时
	// 由 submitToPool 报 ErrStageNotRegistered。
	keys := e.stageKeys()

	// 第零趟之二：依赖接线校验。声明了要用下载器却没接线，就在**启动这一刻**炸掉 ——
	// 不然那句话要等第一条任务跑到那一步才出现，失败的是任务、不是启动（见 deps.go）。
	for _, key := range keys {
		e.checkStageDeps(key, e.stages[key].fetcher)
	}

	// 池子**懒建**：不在这里全量建，等第一条任务 submit 时才建（getOrCreatePool）。于是入口回调
	// 可以放心投给任意阶段 —— 投到谁、谁的池子在这一刻才起，不再有"后面的池子还没建"的 nil 解引用。
	//
	// 引擎在这里**不做策略判断**：有回调、且声明里写了 `Entry: true`，就跑（"这个阶段有没有入口"
	// 由声明层决定 —— 见 internal/app/spec.go；"这一站现在跑不跑"是另一件事，见下面那一句）。
	// 下载器实例先解析好（站点层/阶段层覆盖 → 每阶段一份，见 engine/downloads.go）：第一条任务
	// 可能立刻开跑，解析晚了那几条会拿到全局那份（不至于错，但"声明没生效"这种不一致最难查）。
	e.resolveStageDownloads()
	// 站点快照也在这儿备齐（按声明重建、整块写进落点），后台站点 Tab/站点页读它
	e.buildSiteStats()

	// **先闸住、再投入口任务**（顺序是硬的）：声明了 StartPaused 的站，这一刻整站被暂停 ——
	// 反过来的话，投进去的头几条可能在"闸还没下"的那个窗口里被 worker 抢去跑掉。
	// 闸住之后入队点直接折回溢出表、池子一个都不建（见 applyStartPaused）。
	e.applyStartPaused()
	for _, key := range keys {
		info := e.stages[key]
		if info.submitFunc != nil && info.config.Entry {
			info.submitFunc(e)
		}
	}

	// 启动高水位溢出任务的回灌协程
	e.startDrain()
	// 启动错误队列后台自动轮询（配了 interval 才自动，否则仅手动触发）
	e.startErrorQueue()
	// 启动中断恢复队列（启动即恢复一次，没有定时轮询）
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
	// 空闲池回收（没配 stage_idle_timeout 就是 no-op，池常驻）
	e.startIdleReaper()
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
			// `{dir}/{site}/{stage}/task-{id}-try-{retry}-*` 在目录里找同一次尝试的文件即可。
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
