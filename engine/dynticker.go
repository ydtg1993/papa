package engine

import "time"

// runDynamicTicker 运行一个间隔可动态调整的定时任务。
// getInterval 返回当前生效间隔（<=0 表示停用）；onTick 到点时执行。
// 配置变更（ApplyRuntimeConfig 触发 configChanged 信号）会唤醒它重新读取间隔；
// 只有间隔真正变化时才重建 ticker，避免无关变更（如改浏览器头）反复重置、饿死队列轮询。
func (e *Engine) runDynamicTicker(getInterval func() time.Duration, onTick func()) {
	go func() {
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
			case <-e.configChanged:
				continue
			}
		}
	}()
}
