package engine

import (
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"

	"sort"
)

// SetSiteBreaker registers the site's single outcome detector before Run.
func (e *Engine) SetSiteBreaker(site string, cfg breaker.Config) {
	e.breakers.SetSiteBreaker(site, cfg)
}

// ResumeCrawling retries automatic protection and releases ordinary manual pauses.
func (e *Engine) ResumeCrawling() bool {
	changed := false
	for site := range e.BreakerStatuses() {
		if e.ResumeSite(site) {
			changed = true
		}
	}
	return changed
}

// ResumeSite retries whole-site verification for an active automatic incident.
// It never treats an operator click as health proof. Pure manual pauses release normally.
func (e *Engine) ResumeSite(site string) bool {
	return e.resumeSite(site, false)
}

// ForceResumeSite explicitly bypasses automatic verification. Failure counts and
// historical reports remain; the next failure may immediately protect the site again.
func (e *Engine) ForceResumeSite(site string) bool {
	return e.resumeSite(site, true)
}

func (e *Engine) resumeSite(site string, force bool) bool {
	if !e.knownSite(site) || !e.clearSite(site, force) {
		return false
	}
	e.wakeDrainNow()
	e.writeSiteBreakerState(site, e.pauseInfoOf(site).Paused)
	return true
}

// PauseSite 手动暂停某个站点的抓取；不认识的站点返回 false（后台据此回 400）。
//
// 现场里带上当时的窗口失败数（与今天一致：它是后台 payload 的一个字段，别顺手改成 0）。
func (e *Engine) PauseSite(site, reason string) bool {
	if !e.setSitePausedSignal(site, reason) {
		// 已由人工暂停，无需重复接管。
		return false
	}
	e.writeSiteBreakerState(site, e.pauseInfoOf(site).Paused)
	return true
}

// setSitePausedSignal 只下"整站暂停"的内存信号 + 现场，**不写站点快照**。
//
// 两个调用方：PauseSite（手动暂停，成功后自己写快照）与 applyStartPaused（启动期 StartPaused）。
// 后者不能走 PauseSite：它的暂停态已经由 buildSiteStats 在**一次**快照写里带上了，
// 再逐站写就是 1+N 条 crawler_config upsert（先按"全没暂停"写一次、再逐站改回 true）。
func (e *Engine) setSitePausedSignal(site, reason string) bool {
	if !e.knownSite(site) {
		return false
	}
	return e.setPause(pauseScope{Site: site}, pauseState{
		Reason: reason, Source: PauseSourceManual,
		Failures: e.breakerSum(site),
	})
}

// PauseCrawling 手动闸住抓取（后台/业务都可用）：**所有配了抓取策略的站点**一起暂停，
// 返回是否真的至少停了一个。
//
// 与 PauseSite 一样，对真正停下来的站逐站把快照写回（后台站点表的「已暂停」列读它）；
// 只改内存信号的话，站点表在全局暂停后仍显示「运行中」，两条暂停路径行为不一致。
func (e *Engine) PauseCrawling(reason string) bool {
	sites := e.allSites()
	sort.Strings(sites)
	paused := false
	for _, site := range sites {
		if e.setPause(pauseScope{Site: site}, pauseState{
			Reason: reason, Source: PauseSourceManual,
			Failures: e.breakerSum(site),
		}) {
			paused = true
			e.writeSiteBreakerState(site, e.pauseInfoOf(site).Paused)
		}
	}
	return paused
}

// breakerSum 该站当前窗口内的终态失败数（没配熔断/没这个站都是 0）。
func (e *Engine) breakerSum(site string) int { return e.breakers.breakerSum(site) }

// BreakerStatuses 返回各站点的熔断状态快照：key 是站点 Key（站点是必填维度，没有空键那一档）。
//
// 列出**每个声明的站点**（不管有没有单配阈值、有没有在暂停）。**一条都不能少**，
// 包括没启用、也没暂停的：后台的「恢复抓取」端点 targets 取自这张表，不列就等于
// "那个站暂停了却既看不见、也点不到恢复"，只能重启或让业务代码自己调 ResumeCrawling。
func (e *Engine) BreakerStatuses() map[string]core.BreakerStatus {
	sites := e.allSites()
	sort.Strings(sites)
	out := make(map[string]core.BreakerStatus, len(sites))
	for _, site := range sites {
		out[site] = e.breakerStatusOf(site)
	}
	return out
}

// breakerStatusOf 一个站点的快照：**合成视图** —— 统计与配置取自熔断器，暂停那几列取自暂停信号。
//
// 为什么合成：暂停状态现在只有一处（pause.go），而后台的历史契约是"一条记录里既有熔断阈值
// 又有暂停现场"。合成让 payload 的字段形状一个字都不变（只多一个 Source）。
func (e *Engine) breakerStatusOf(site string) core.BreakerStatus {
	e.breakers.breakerMu.Lock()
	defer e.breakers.unlockAndEmit()
	return e.breakerStatusOfLocked(site)
}

// breakerStatusOfLocked 调用方持有 breakerMu，快照不会被恢复/新熔断拆开。
func (e *Engine) breakerStatusOfLocked(site string) core.BreakerStatus {
	st := e.breakers.statusLocked(site)

	// 用"该站任一暂停条目"而不是只查整站那条：阶段级暂停也该在这一行里显示出来
	//（这样才能一眼看出"这一站停的是哪个阶段"）。
	info := e.pauseInfoOf(site)
	if info.Paused {
		st.Paused = true
		st.PausedAt = info.State.At
		st.Reason = info.State.Reason
		st.Source = info.State.Source
		// stage 先说**scope**（阶段级暂停停的是它），scope 是整站时才退回触发现场那个阶段：
		// 熔断触发一定带"当时哪个阶段在失败"，手动整站暂停两者都为空。
		st.Stage = info.Scope.Stage
		if st.Stage == "" {
			st.Stage = info.State.Stage
		}
		st.Failures = info.State.Failures
	}
	st.ResumedAt = info.ResumedAt
	return st
}
