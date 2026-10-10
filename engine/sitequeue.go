package engine

import "github.com/ydtg1993/papa/v3/config"

// 站点级的队列生效配置。声明层（`internal/app`）把站点声明里那三个治理队列解析好，
// 通过 SetSiteQueues 交进来；引擎这边只做一件事：按站点取。
//
// **没有登记过的站点 = 零值 = 三队列都不跑**（后台那几行显示"已停用"，手动入口照旧）—— 两个作用：
// ① 未归属（`site` 为空）的任务没有站点声明可挂，三队列对它一律不跑；
// ② 手搓 Engine 的测试、以及引擎不认识的站点，不会凭空拿到一份"默认配置"。
//
// 这里**没有运行期覆盖层**了：队列参数只在启动时读一次（改配置 = 改站点声明 + 重启）。
type SiteQueues struct {
	Error   config.ErrorQueueConfig
	Recover config.RecoverQueueConfig
	Repeat  config.RepeatQueueConfig
}

// SetSiteQueues 登记某站点的队列生效配置（App.RegisterSites 调，必须在 ApplyRegisterStage 之前）。
// site 为空是非法参数：那正是"未归属"（默认 scope），三条队列对它一律不跑 —— 要给未归属的任务治理，
// 先给这一站一个 Key（App.RegisterSites 对这种情况直接 panic 并这么说）。
func (e *Engine) SetSiteQueues(site string, q SiteQueues) {
	if site == "" {
		return
	}
	e.queueCfgMu.Lock()
	defer e.queueCfgMu.Unlock()
	if e.siteQueues == nil {
		e.siteQueues = make(map[string]SiteQueues)
	}
	e.siteQueues[site] = q
}

// siteQueuesOf 取某站点的队列配置（只读；没登记返回 false）。
func (e *Engine) siteQueuesOf(site string) (SiteQueues, bool) {
	e.queueCfgMu.RLock()
	defer e.queueCfgMu.RUnlock()
	q, ok := e.siteQueues[site]
	return q, ok
}

// errorQueueConfig 返回该站点错误队列的生效配置（**没登记过 = 零值 = 不跑**）。
func (e *Engine) errorQueueConfig(site string) config.ErrorQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Error
	}
	return config.ErrorQueueConfig{}
}

// recoverQueueConfig 返回该站点启动恢复的生效配置（**没登记过 = 零值 = 不跑**）。
func (e *Engine) recoverQueueConfig(site string) config.RecoverQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Recover
	}
	return config.RecoverQueueConfig{}
}

// repeatQueueConfig 返回该站点周期轮询队列的生效配置（**没登记过 = 零值 = 不跑**）。
//
// 这里的 `Enabled` 是**声明层**的"这一站有没有这条队列"；本站"要不要**自动**轮询"是另一层 ——
// 站点声明的 `AutoRepeat`（`siteAutoRepeat` → `core.Site.AutoRepeat`，落库、后台可运行期开停）。
// 两者是**与**关系。
func (e *Engine) repeatQueueConfig(site string) config.RepeatQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Repeat
	}
	return config.RepeatQueueConfig{}
}
