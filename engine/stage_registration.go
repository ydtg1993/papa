package engine

import (
	"github.com/ydtg1993/papa/v4/internal/track"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"sort"
	"time"
)

// Stage assembly is Engine lifecycle orchestration; mutable pool membership,
// delayed work and overflow are owned by their scheduler components.
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

// buildPool 建一个阶段的工作池并登记：建池 + 按站点绑闸门 + 起 worker + 建监控 stats。
// 调用方须保证 key（`core.StageKey`，不是裸阶段名）已注册。
func (e *Engine) buildPool(key string, info *stageInfo) *workerpool.WorkerPool[*Task] {
	cfg := info.config
	pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
	if pool == nil {
		e.loggerSet.Engine.Errorf("cannot build stage %q: %v", key, workerpool.ValidateConfig(cfg.WorkerCount, cfg.QueueSize))
		return nil
	}
	// 闸门绑定阶段 scope；启用预算时允许取出暂停任务，由 stageHandler 在业务执行前交回持久待执行集合。
	// 旧的无预算内部夹具仍在取任务点等待，所以"整站暂停"与
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
	created := false
	pool := e.pools.GetOrCreate(key, func() *workerpool.WorkerPool[*Task] {
		p := e.buildPool(key, info)
		created = p != nil
		return p
	})
	// Registration and shutdown exclusion finish before external emitter code.
	// Only the caller whose factory ran publishes creation; cache hits and refused
	// builds do not emit. A synchronous observer can now inspect or stop the pool.
	if created {
		e.emit(events.Event{Type: events.TypeStagePool, Site: info.config.Site, Stage: info.stage,
			Source: "created", Counts: map[string]int{"workers": info.config.WorkerCount}})
	}
	return pool
}

// poolForwarder 一条错误转发协程的归属：停机信号 + 它转发的是哪个池。
//
// 取消时**必须核对池身份**，不能只按 stage key：reapIdle 把池从登记表摘掉之后、调用
// cancelPoolErrorForwarder 之前，同阶段可能已被重新提交、建了新池新转发协程 —— 只按 key
// 关通道会误关新池的那条，让新池的错误再也进不了 Errors()。
type poolForwarder struct {
	stop chan struct{}
	pool *workerpool.WorkerPool[*Task]
}

// startPoolErrorForwarder 起一条转发协程，把池的错误通道汇进引擎的常驻错误通道（Errors()）。
// pool.Errors() 从不 close，所以必须带停机信号 —— 池被 reap 时 close 它，否则协程泄漏到引擎结束。
func (e *Engine) startPoolErrorForwarder(key string, pool *workerpool.WorkerPool[*Task]) {
	stop := make(chan struct{})
	e.mu.Lock()
	if e.errorForwardCancel == nil {
		e.errorForwardCancel = make(map[string]*poolForwarder)
	}
	if old, ok := e.errorForwardCancel[key]; ok {
		close(old.stop) // 旧池的转发协程该退了（池被重建）
	}
	e.errorForwardCancel[key] = &poolForwarder{stop: stop, pool: pool}
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
// 只停「仍属于被回收的那份池」的转发协程：reap 与新池建之间没有锁，同阶段可能已换了池，
// 只按 key 关会误关新池的（见 poolForwarder 的注释）。
func (e *Engine) cancelPoolErrorForwarder(key string, pool *workerpool.WorkerPool[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if fw, ok := e.errorForwardCancel[key]; ok && fw.pool == pool {
		close(fw.stop)
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
				for key, pool := range e.pools.ReapIdle(idle, e.cfg.Crawler.StopTimeoutOrDefault()) {
					e.harvestStageTotals(key, pool)
					e.cancelPoolErrorForwarder(key, pool)
					e.loggerSet.Engine.Debugf("reaped idle stage pool: %s", key)
					site, stage := splitStageKey(key)
					e.emit(events.Event{Type: events.TypeStagePool, Site: site, Stage: stage, Source: "reaped"})
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

	e.schedulerStarted = true
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
	// 运行事件流的阶段/队列周期快照（obs.events 开启且配了 interval 才起）
	e.startSnapshotEmitter()
	// 空闲池回收（没配 stage_idle_timeout 就是 no-op，池常驻）
	e.startIdleReaper()
}
