package engine

import (
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/core"
)

// sitesKeyName 站点快照在 crawler_config 里的键（admin/configstore 的 KeySites ——
// 但本包不 import admin/*，所以只借这个名字打日志）。
const sitesKeyName = "sites"

// 站点快照：**内存是源，库是投影**。
//
// 这份数据只有两类 —— ①声明里抄来的（Key / BaseURL / 阶段数）②运行期的暂停状态。两者的真源
// 都在引擎里（声明 + 暂停信号），落库那一份是给"要 SQL 查的运行期状态"留的投影
// （papa 把它放在 `crawler_config` 的 `sites` 键上，见 admin/configstore）。
//
// 落点通过 SiteStore 注入，所以本包不 import admin/*，也不认识那张表：
//   - `Sites()` 读快照（内存优先，命中不查库）—— 监控页 3 秒一刷走这条；
//   - `SetSites()` 整块写（启动播种）；
//   - `UpdateSites()` 在**落点那一层的写锁里**读-改-写（暂停/恢复）—— 整块 JSON 的 RMW 必须原子，
//     否则两次并发改不同站点会互相盖掉。
type SiteStore interface {
	Sites() ([]core.SiteStat, error)
	SetSites(rows []core.SiteStat) error
	UpdateSites(fn func(rows []core.SiteStat) []core.SiteStat) error
}

// SetSiteStore 注入站点快照的落点。**由 internal/app 在造引擎后立刻调一次**；
// 没注入时站点相关的能力退化成"空快照 + 不落库"（手工拼的 Engine、部分单测）。
func (e *Engine) SetSiteStore(s SiteStore) { e.siteStore = s }

// siteKeys 所有站点键，按字典序。**没有"默认 scope"那一档**：站点是必填维度 ——
// 声明必须有 Key（validateSites 拒没有 Key 的声明）、任务必须有 site（SubmitTask 拒空串）。
func (e *Engine) siteKeys() []string {
	e.breakerMu.Lock()
	keys := make([]string, 0, len(e.sites))
	for k := range e.sites {
		keys = append(keys, k)
	}
	e.breakerMu.Unlock()
	sort.Strings(keys)
	return keys
}

// stageCountOfSite 本站声明了几个阶段（站点快照里那一列的来源）。
func (e *Engine) stageCountOfSite(site string) int {
	n := 0
	for _, info := range e.stages {
		if info.config.Site == site {
			n++
		}
	}
	return n
}

// buildSiteStats 启动期把站点快照备齐（单线程段调用）：**读 `crawler_config` 的 `sites` 那一行 ——
// 没有就按声明生成，有就把值放进内存**（这次读会把内存缓存也填上，之后监控页与站点页都走内存）。
//
// 读回来之后要**按当前声明对齐**，因为声明是站点集合与那两个字段的源：
//   - 声明里有、行里没有的站 → 补上；
//   - 行里有、声明里已经删掉的站 → 丢掉（否则后台会显示一个不存在的站）；
//   - `base_url` / `stage_count` → 按声明刷新（文档里那句"改声明要重启、直接改库不生效"就是这条）；
//   - `breaker_paused*` → **Reset 后按声明算**：普通站一律"没暂停"（暂停状态不跨重启）；
//     声明了 `StartPaused` 的站则带 paused=true + 本次启动时刻 —— 它启动就该是停着的。
//     这一份就是"启动后应有的样子"，所以随后 applyStartPaused 只下内存信号、**不再逐站写库**。
//
// 对齐之后内容一致就不回写（一次正常启动不该让 `updated_at` 往前跳），不一致（或那一行压根不在）
// 才写一次。落点读写失败都不让启动失败（与"读不到配置就用声明"同一取向）：内存缓存里那份仍然是
// 声明算出来的，只是库里的投影暂时落后，后台「站点」页与外部 SQL 看到的是上一次的样子。
func (e *Engine) buildSiteStats() {
	if e.siteStore == nil {
		return
	}
	want := e.declaredSiteStats()

	cur, err := e.siteStore.Sites() // 没有这一行时是空切片，不会报错
	if err != nil {
		e.loggerSet.Engine.Warnf("load sites snapshot: %s（按声明走，库里那份这次没读到）", err.Error())
		return
	}
	switch {
	case len(cur) == 0:
		e.loggerSet.Engine.Infof("站点快照：crawler_config 里没有 %s 这一行，按声明生成 %d 条", sitesKeyName, len(want))
	case startPausedResetOnly(cur, want):
		// 上次启动留下的 paused=true 与本次声明的 StartPaused 本来就该一致，只是暂停时间要按本次启动刷新。
		// 这不是"声明漂移"，别用「字段变了」吓人。
		e.loggerSet.Engine.Infof("站点快照：按 StartPaused 重新暂停 %d 个站点（暂停时间按本次启动刷新），写回一次", countPausedSites(want))
	case !sameSiteStats(cur, want):
		e.loggerSet.Engine.Infof("站点快照：库里那份与当前声明不一致（%s），已按声明对齐 %d 条",
			siteStatsDiff(cur, want), len(want))
	default:
		e.loggerSet.Engine.Infof("站点快照：已从 crawler_config 载入 %d 条（与声明一致）", len(cur))
		return // 一致：不动库
	}

	if err := e.siteStore.SetSites(want); err != nil {
		e.loggerSet.Engine.Warnf("save sites snapshot: %s（后台「站点」页与外部 SQL 看到的是上一次的快照）", err.Error())
	}
}

// declaredSiteStats 按声明算出来的那一份快照：`StartPaused` 的站直接带 paused=true + 本次启动时刻。
//
// 为什么不等 applyStartPaused 逐站写：那会先按"全没暂停"写一份、再对每个 StartPaused 站各写一次，
// 一次启动 1+N 条 crawler_config 写（先清零、再改回 true，纯浪费，日志里也能看到三次大 JSON upsert）。
func (e *Engine) declaredSiteStats() []core.SiteStat {
	paused := make(map[string]bool)
	for _, key := range e.startPausedSites() {
		paused[key] = true
	}
	now := time.Now()
	rows := make([]core.SiteStat, 0, len(e.sites))
	for _, key := range e.siteKeys() {
		site, _ := e.Site(key) // 取不到就是零值（声明里有、引擎里还没登记）
		row := core.SiteStat{
			Key:        key,
			BaseURL:    site.BaseURL,
			StageCount: e.stageCountOfSite(key),
		}
		if paused[key] {
			row.BreakerPaused = true
			row.BreakerPausedAt = now
			row.PauseSource = "startup"
			row.PauseReason = "站点声明 StartPaused"
		}
		rows = append(rows, row)
	}
	return rows
}

// countPausedSites 快照里 paused=true 的站点数（启动日志用）。
func countPausedSites(rows []core.SiteStat) int {
	n := 0
	for _, r := range rows {
		if r.BreakerPaused {
			n++
		}
	}
	return n
}

// startPausedResetOnly 判断两份快照的差异是否**只是 StartPaused 站的暂停时间**：
// 站集合 / BaseURL / StageCount / 暂停标记都一致，只有 BreakerPausedAt 不同。
// 这是每次启动按声明重新暂停的正常结果，不是声明漂移 —— 日志该说的是"重新暂停"，不是"字段变了"。
func startPausedResetOnly(cur, want []core.SiteStat) bool {
	if len(cur) != len(want) {
		return false
	}
	have := make(map[string]core.SiteStat, len(cur))
	for _, r := range cur {
		have[r.Key] = r
	}
	onlyPausedAt := false
	for _, w := range want {
		got, ok := have[w.Key]
		if !ok || got.BaseURL != w.BaseURL || got.StageCount != w.StageCount ||
			got.BreakerPaused != w.BreakerPaused {
			return false
		}
		if !got.BreakerPausedAt.Equal(w.BreakerPausedAt) {
			onlyPausedAt = true
		}
	}
	return onlyPausedAt
}

// sameSiteStats 两份快照内容是否一致（按站点键比；两份都按站点键升序，见 siteKeys / configstore.Sites）。
func sameSiteStats(a, b []core.SiteStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// siteStatsDiff 一句话说清哪里不一样（给启动日志用）：多了哪些站、少了哪些站、哪些字段变了。
func siteStatsDiff(cur, want []core.SiteStat) string {
	have := make(map[string]core.SiteStat, len(cur))
	for _, r := range cur {
		have[r.Key] = r
	}
	var missing, extra, changed []string
	for _, r := range want {
		got, ok := have[r.Key]
		switch {
		case !ok:
			missing = append(missing, r.Key)
		case got.BaseURL != r.BaseURL || got.StageCount != r.StageCount:
			changed = append(changed, r.Key)
		case got.BreakerPaused || !got.BreakerPausedAt.IsZero():
			changed = append(changed, r.Key+"(暂停态)")
		}
	}
	for _, r := range cur {
		if _, ok := lookupSite(want, r.Key); !ok {
			extra = append(extra, r.Key)
		}
	}
	parts := make([]string, 0, 3)
	if len(missing) > 0 {
		parts = append(parts, "新增/缺失 "+strings.Join(missing, ","))
	}
	if len(extra) > 0 {
		parts = append(parts, "声明里已没有 "+strings.Join(extra, ","))
	}
	if len(changed) > 0 {
		parts = append(parts, "字段变了 "+strings.Join(changed, ","))
	}
	if len(parts) == 0 {
		return "顺序或条数"
	}
	return strings.Join(parts, "；")
}

// lookupSite 在快照里找一个站点。
func lookupSite(rows []core.SiteStat, key string) (core.SiteStat, bool) {
	for _, r := range rows {
		if r.Key == key {
			return r, true
		}
	}
	return core.SiteStat{}, false
}

// GetSiteStats 返回各站点的快照（内存优先，命中不查库）。监控页 3 秒一刷走这条。
func (e *Engine) GetSiteStats() map[string]core.SiteStat {
	if e.siteStore == nil {
		return map[string]core.SiteStat{}
	}
	rows, err := e.siteStore.Sites()
	if err != nil {
		e.loggerSet.Engine.Warnf("load sites snapshot: %s", err.Error())
		return map[string]core.SiteStat{}
	}
	out := make(map[string]core.SiteStat, len(rows))
	for _, r := range rows {
		st := e.breakerStatusOf(r.Key)
		if st.Paused {
			r.PauseSource, r.PauseReason = st.Source, st.Reason
			if st.Reason == "站点声明 StartPaused" {
				r.PauseSource = "startup"
			}
		}
		out[r.Key] = r
	}
	return out
}

// writeSiteBreakerState 暂停/恢复时把那一站的状态写进快照（整块替换，暂停时间 = 那一刻）。
//
// 读-改-写在**落点那一层的写锁**里完成（`UpdateSites`）：这里两次调用并发改不同站点时，
// 后写的那次不能把前一次的结果抹平 —— 值是一整块 JSON，RMW 分两次加锁就有这个窗口。
//
// 落点写失败只记 WARN：暂停/恢复是**控制面**动作，内存（暂停信号）才是闸门的判据 ——
// 不该因为"给外部看的投影"写不进去就停不下来。真要强一致（写库失败 = 操作失败），
// 把这里的错误往上抛给 PauseSite 即可，但那会让 DB 抖动变成"停不住"。
func (e *Engine) writeSiteBreakerState(site string, paused bool) {
	if e.siteStore == nil {
		return
	}
	at := time.Now()
	st := e.breakerStatusOf(site)
	err := e.siteStore.UpdateSites(func(rows []core.SiteStat) []core.SiteStat {
		for i := range rows {
			if rows[i].Key != site {
				continue
			}
			rows[i].BreakerPaused = paused
			if paused {
				rows[i].BreakerPausedAt = at
				rows[i].PauseSource, rows[i].PauseReason = st.Source, st.Reason
			} else {
				rows[i].BreakerPausedAt = time.Time{}
				rows[i].PauseSource, rows[i].PauseReason = "", ""
			}
		}
		return rows
	})
	if err != nil {
		e.loggerSet.Engine.Warnf("save sites breaker state for %q: %s", site, err.Error())
	}
}
