package engine

import "github.com/ydtg1993/papa/v3/config"

// 站点级的队列生效配置。声明层（`internal/app`）把"全局那份 + 站点声明里的覆盖"解析好，
// 通过 SetSiteQueues 交进来；引擎这边只做一件事：按站点取。
//
// **没有登记过的站点回退全局那份**（`e.cfg.ErrorQueue` 等）—— 两个作用：
// ① 默认 scope（Key 为空）本来就用全局那份，不必也不允许单独配；
// ② 手搓 Engine 的测试、以及引擎不认识的站点，行为与以前完全一致。
//
// 这里**没有运行期覆盖层**了：队列参数只在启动时读一次（改配置 = 改代码 + 重启）。
type SiteQueues struct {
	Error   config.ErrorQueueConfig
	Recover config.RecoverQueueConfig
	Repeat  config.RepeatQueueConfig
}

// SetSiteQueues 登记某站点的队列生效配置（App.RegisterSites 调，必须在 ApplyRegisterStage 之前）。
// site 为空是非法参数（那正是"默认 scope"，它用全局那份）。
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

// errorQueueConfig 返回该站点错误队列的生效配置（没配过 = 全局那份）。
func (e *Engine) errorQueueConfig(site string) config.ErrorQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Error
	}
	return e.cfg.ErrorQueue
}

// recoverQueueConfig 返回该站点启动恢复的生效配置（没配过 = 全局那份）。
func (e *Engine) recoverQueueConfig(site string) config.RecoverQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Recover
	}
	return e.cfg.RecoverQueue
}

// repeatQueueConfig 返回该站点周期轮询队列的生效配置（没配过 = 全局那份）。
//
// 本站"要不要自动轮询"**不在**这份配置里：那是站点声明的 `AutoRepeat`
// （`siteAutoRepeat` → `core.Site.AutoRepeat`），与这里的 `Enabled`（全局总开关）是**与**关系。
func (e *Engine) repeatQueueConfig(site string) config.RepeatQueueConfig {
	if q, ok := e.siteQueuesOf(site); ok {
		return q.Repeat
	}
	return e.cfg.RepeatQueue
}
