package engine

import "time"

// defaultWakeCoalesce 是动态 ticker 的唤醒合并窗口（见 Engine.wakeCoalesce）。
//
// 启动恢复会连续重投几百条任务、每条都可能在 submitToPool 里 wakeRepeatQueue；动态 ticker
// 每醒一次就要重算一次节拍，而轮询队列的节拍是一条 `MIN(...)` 聚合查询。窗口内把同一批
// wake 合并成一次重算，把"N 条任务 → N 次聚合查询"压成 1 次。代价：孤立的一次 wake 最多
// 晚一个窗口才被算进节拍（250ms 对分钟级/小时级的轮询间隔无感）。
const defaultWakeCoalesce = 250 * time.Millisecond

// runDynamicTicker 运行一个间隔可动态调整的定时任务。
// getInterval 返回当前生效间隔（<=0 表示停用）；onTick 到点时执行。
// 只有间隔真正变化时才重建 ticker（避免无关唤醒反复重置、饿死队列轮询）。
//
// wake 是额外的"数据变了，重算间隔"通道（可传 nil，nil channel 永不触发）—— 队列的间隔是
// **pull** 出来的：只在 tick 到点或收到信号时重算。轮询队列的节拍还取决于表里的数据
// （最早一条到点），所以新提交一条任务、后台改了某条的周期、把它开起来，都得显式叫它一声，
// 否则要等当前那次 sleep 到期（可能是全局的几小时）才被看见。
func (e *Engine) runDynamicTicker(getInterval func() time.Duration, wake <-chan struct{}, onTick func()) {
	// 进等待组：Stop 要等 ticker 退出再返回。否则它可能在"某一轮队列执行的中途写库"，
	// 而调用方看到 drained=true 就去关库，那些写入全部报 `sql: database is closed`。
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		var ticker *time.Ticker
		var tickerC <-chan time.Time
		var tickerInterval time.Duration
		defer func() {
			if ticker != nil {
				ticker.Stop()
			}
		}()

		for {
			interval := getInterval()
			if interval <= 0 {
				// 停用：停止 ticker，等待配置变更或引擎结束
				if ticker != nil {
					ticker.Stop()
					ticker = nil
					tickerC = nil
					tickerInterval = 0
				}
				select {
				case <-e.ctx.Done():
					return
				case <-wake: // 数据变了：重新算间隔（可能是 0 = 仍然停用）
					continue
				}
			}
			// 只有间隔变化（或首次）才重建 ticker
			if ticker == nil || tickerInterval != interval {
				if ticker != nil {
					ticker.Stop()
				}
				ticker = time.NewTicker(interval)
				tickerC = ticker.C
				tickerInterval = interval
			}
			select {
			case <-e.ctx.Done():
				return
			case <-tickerC:
				onTick()
			case <-wake:
				if e.wakeCoalesce > 0 && !e.coalesceWake(wake, tickerC, onTick) {
					return
				}
				continue
			}
		}
	}()
}

// coalesceWake 合并一次 wake 之后紧跟的同批唤醒：等一个 wakeCoalesce 窗口再让外层重算节拍。
//
// 窗口内到点的 tick **照常执行** —— 那是"队列真的到点了"，不能被合并吞掉；
// 只有"数据变了，重算间隔"这件事被合并。返回 false 表示 ctx 已结束。
func (e *Engine) coalesceWake(wake <-chan struct{}, tickerC <-chan time.Time, onTick func()) bool {
	timer := time.NewTimer(e.wakeCoalesce)
	defer timer.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-wake:
			// 同一批（一次启动恢复 / 一次批量提交）里的后续唤醒：吞掉，窗口到期统一重算一次。
		case <-tickerC:
			onTick()
		}
	}
}
