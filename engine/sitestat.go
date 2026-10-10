package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm/clause"
)

// 站点表与站点快照（见 models/crawler_site.go 与 core.SiteStat）。
//
// 分工：**库是站点信息与慢变统计的落点**（后台「站点」页与站点 Tab 的数据源），
// 内存里那份（e.siteStats）供监控页 3 秒一刷 —— 与治理队列同一套：请求路径不查库。
// 写入点只有三处，都不在监控请求路径上：启动播种、每站轮询跑完、熔断暂停/恢复。

// siteKeys 所有站点键：默认 scope（空串）在最前，其余按站点名字典序。
// 默认 scope 永远在列 —— `crawler_tasks.site` 是后加的列，更早入库的行是空串，
// 它们也占一个"站点维度"（后台那一行叫「默认 scope」）。
func (e *Engine) siteKeys() []string {
	e.breakerMu.Lock()
	keys := make([]string, 0, len(e.sites))
	for k := range e.sites {
		keys = append(keys, k)
	}
	e.breakerMu.Unlock()
	sort.Strings(keys)
	return append([]string{""}, keys...)
}

// stageCountOfSite 本站声明了几个阶段（站点表里那一列的来源）。
func (e *Engine) stageCountOfSite(site string) int {
	n := 0
	for _, info := range e.stages {
		if info.config.Site == site {
			n++
		}
	}
	return n
}

// seedSiteRows 启动期按声明把站点行备齐（单线程段调用，紧跟 ensureRepeatQueues）。
//
// 规则与任务周期一致：**声明只播种** —— 行不存在就按声明插入（含 AutoRepeat）；
// 已存在则只刷新 base_url / stage_count，**不动 AutoRepeat**（运营在后台改过的要留住，库为事实）。
// 读库失败（比如还没跑 migrate）不让启动失败：内存快照退化成"只有声明里的信息"，
// 并留一条 Error —— 框架启动时本来就有"该有的表在不在"的检查，这条只是把原因说清楚。
func (e *Engine) seedSiteRows() {
	existing := map[string]models.CrawlerSite{}
	var rows []models.CrawlerSite
	if err := e.db.Find(&rows).Error; err != nil {
		e.loggerSet.Engine.Errorf("load crawler_sites: %s（先跑一次 `papa migrate` 建表；这次启动站点信息按声明走）", err.Error())
	} else {
		for _, r := range rows {
			existing[r.Key] = r
		}
	}

	next := make(map[string]core.SiteStat, len(e.sites)+1)
	for _, key := range e.siteKeys() {
		site, _ := e.Site(key) // 默认 scope 取不到 → 零值
		stat := core.SiteStat{
			Key:        key,
			BaseURL:    site.BaseURL,
			StageCount: e.stageCountOfSite(key),
			// 默认 scope 没有站点声明：它只能靠全局 repeat_queue.enabled 关，这里恒 true
			AutoRepeat: key == "" || site.AutoRepeat,
		}
		row, exists := existing[key]
		switch {
		case !exists:
			created := models.CrawlerSite{
				Key: key, BaseURL: stat.BaseURL,
				AutoRepeat: stat.AutoRepeat, StageCount: stat.StageCount,
			}
			// 两个进程同时首启时，唯一键冲突就跳过（另一进程已经建好了），不必报错
			if err := e.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&created).Error; err != nil {
				e.loggerSet.Engine.Errorf("create crawler_sites row %q: %s", key, err.Error())
			}
		default:
			stat.AutoRepeat = row.AutoRepeat // 库为事实：声明不再改它
			if err := e.db.Model(&models.CrawlerSite{}).Where("key = ?", key).
				Updates(map[string]any{"base_url": stat.BaseURL, "stage_count": stat.StageCount}).Error; err != nil {
				e.loggerSet.Engine.Warnf("refresh crawler_sites row %q: %s", key, err.Error())
			}
		}
		stat.LastRepeatAt = timeOf(row.LastRepeatAt)
		stat.RepeatTotal = row.RepeatTotal
		stat.RepeatBacklog = row.RepeatBacklog
		stat.LastRepeatError = row.LastRepeatError
		stat.BreakerPaused = row.BreakerPaused
		stat.BreakerPausedAt = timeOf(row.BreakerPausedAt)
		next[key] = stat
	}

	e.siteMu.Lock()
	e.siteStats = next
	e.siteMu.Unlock()
}

// GetSiteStats 返回各站点的快照（纯内存，供监控页高频拉取）。
func (e *Engine) GetSiteStats() map[string]core.SiteStat {
	e.siteMu.RLock()
	defer e.siteMu.RUnlock()
	out := make(map[string]core.SiteStat, len(e.siteStats))
	for k, v := range e.siteStats {
		out[k] = v
	}
	return out
}

// writeSiteRepeatStats 某一站的轮询队列跑完一轮后，把统计写回站点表 + 内存快照。
// 先改内存再写库：库抖一下不该让后台看不到刚刚那轮的结果。
//
// 按列 `Updates`（不用 `Save`）：站点行上还有运营改过的列，整行写回会把它们盖掉。
func (e *Engine) writeSiteRepeatStats(site string, total int64, backlog int, runErr error) {
	now := time.Now()
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}

	e.siteMu.Lock()
	stat, ok := e.siteStats[site]
	if ok {
		stat.LastRepeatAt = now
		stat.RepeatTotal = total
		stat.RepeatBacklog = int64(backlog)
		stat.LastRepeatError = errMsg
		e.siteStats[site] = stat
	}
	e.siteMu.Unlock()
	if !ok {
		return // 站点表里没有这一行（没播种过 / 还没 migrate）：只更新内存那半边，别写库报错
	}

	if err := e.db.Model(&models.CrawlerSite{}).Where("key = ?", site).Updates(map[string]any{
		"last_repeat_at":    now,
		"repeat_total":      total,
		"repeat_backlog":    backlog,
		"last_repeat_error": errMsg,
	}).Error; err != nil {
		e.loggerSet.Engine.Warnf("save crawler_sites repeat stats for %q: %s", site, err.Error())
	}
}

// SetSiteAutoRepeat 运行期开/停某站点的**自动轮询**（后台「站点」页的那两个动作走它）。
//
// 只改 `crawler_sites.auto_repeat` 这一列 + 内存快照，然后叫醒该站的轮询队列 —— 闸门在 onTick 里
// 判（见 startRepeatQueue），所以"开"要叫这一声它才会立刻投，不然要等当前那次 sleep 到期。
//
// 默认 scope（Key 为空）**不允许**在这里改：它没有站点声明可挂，只能靠全局
// `crawler.repeat_queue.enabled` —— 这条与"站点行只播种声明值"是同一套约定。
func (e *Engine) SetSiteAutoRepeat(key string, on bool) error {
	if key == "" {
		return ErrDefaultScopeNoAuto
	}
	// 这一列当前值就是版本守卫（要开就必须现在关着，反之亦然）：重复点击 0 行
	res := e.db.Model(&models.CrawlerSite{}).
		Where("key = ? AND auto_repeat = ?", key, !on).
		Update("auto_repeat", on)
	if res.Error != nil {
		return fmt.Errorf("set site %q auto_repeat=%v: %w", key, on, res.Error)
	}
	if res.RowsAffected == 0 {
		return e.whySiteAutoRejected(key, on)
	}

	e.siteMu.Lock()
	if stat, ok := e.siteStats[key]; ok {
		stat.AutoRepeat = on
		e.siteStats[key] = stat
	}
	e.siteMu.Unlock()
	e.wakeRepeatQueue(key)
	return nil
}

// whySiteAutoRejected 条件更新影响 0 行时，回查一次把原因说清楚。
func (e *Engine) whySiteAutoRejected(key string, on bool) error {
	e.siteMu.RLock()
	stat, ok := e.siteStats[key]
	e.siteMu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSite, key)
	}
	if stat.AutoRepeat == on {
		if on {
			return ErrSiteAlreadyAuto
		}
		return ErrSiteAlreadyManual
	}
	return ErrTaskChanged // 有人同时把它改成了相反值，这一方输了
}

// writeSiteBreakerState 熔断暂停/恢复时写站点表（暂停时间 = 那一刻；恢复时清掉）。
func (e *Engine) writeSiteBreakerState(site string, paused bool) {
	e.siteMu.Lock()
	stat, ok := e.siteStats[site]
	if ok {
		stat.BreakerPaused = paused
		if paused {
			stat.BreakerPausedAt = time.Now()
		} else {
			stat.BreakerPausedAt = time.Time{}
		}
		e.siteStats[site] = stat
	}
	e.siteMu.Unlock()
	if !ok {
		return
	}

	updates := map[string]any{"breaker_paused": paused, "breaker_paused_at": nil}
	if paused {
		updates["breaker_paused_at"] = time.Now()
	}
	if err := e.db.Model(&models.CrawlerSite{}).Where("key = ?", site).Updates(updates).Error; err != nil {
		e.loggerSet.Engine.Warnf("save crawler_sites breaker state for %q: %s", site, err.Error())
	}
}

// timeOf 把可空时间折成零值（DTO 用零值表示"从未"，前端据年份判断）。
func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
