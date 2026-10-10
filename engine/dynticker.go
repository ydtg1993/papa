package engine

import "time"

// runDynamicTicker 运行一个间隔可动态调整的定时任务。
// getInterval 返回当前生效间隔（<=0 表示停用）；onTick 到点时执行。
// 配置变更（ApplyRuntimeConfig 触发 configChanged 信号）会唤醒它重新读取间隔；
// 只有间隔真正变化时才重建 ticker，避免无关变更（如改浏览器头）反复重置、饿死队列轮询。
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
				case <-e.configChanged:
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
				continue
			case <-e.configChanged:
				continue
			}
		}
	}()
}
