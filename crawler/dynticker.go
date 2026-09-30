package crawler

import "time"

// runDynamicTicker 运行一个间隔可动态调整的定时任务。
// getInterval 返回当前生效间隔（<=0 表示停用）；onTick 到点时执行。
// 配置变更（ApplyRuntimeConfig 触发 configChanged 信号）会唤醒它重新读取间隔，实现 enabled/interval 运行期热更。
func (e *Engine) runDynamicTicker(getInterval func() time.Duration, onTick func()) {
	go func() {
		for {
			interval := getInterval()
			if interval <= 0 {
				// 停用：等待配置变更或引擎结束
				select {
				case <-e.ctx.Done():
					return
				case <-e.configChanged:
					continue
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-e.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				onTick()
			case <-e.configChanged:
				timer.Stop()
			}
		}
	}()
}
