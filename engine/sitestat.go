package engine

import (
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 站点表与站点快照（见 models/crawler_site.go 与 core.SiteStat）。
//
// 分工：**库是站点信息与慢变统计的落点**（后台「站点」页与站点 Tab 的数据源），
// 内存里那份（e.siteStats）供监控页 3 秒一刷 —— 与治理队列同一套：请求路径不查库。
// 写入点只有两处，都不在监控请求路径上：启动播种、熔断暂停/恢复。

// siteKeys 所有站点键，按字典序。**没有"默认 scope"那一档**：站点是必填维度 ——
// 声明必须有 Key（validateSites 拒没有 Key 的声明）、任务必须有 site（SubmitTask 拒空串），
// 所以这张表里只会是声明过的站点。
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

// siteKeyScope 定位某个站点行的条件。**`key` 是 MySQL 保留字**：裸写进 SQL 会被当成关键字，
// 整条语句报 Error 1064（真库实测，见 engine/sqlshape_test.go）—— 引号只写在这一处，各调用点共用。
func siteKeyScope(db *gorm.DB, key string) *gorm.DB {
	return db.Model(&models.CrawlerSite{}).Where("`key` = ?", key)
}

// seedSiteRows 启动期按声明把站点行备齐（单线程段调用）。
//
// 规则：**声明只播种** —— 行不存在就按声明插入；已存在则只刷新 base_url / stage_count。
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

	next := make(map[string]core.SiteStat, len(e.sites))
	for _, key := range e.siteKeys() {
		site, _ := e.Site(key) // 取不到就是零值（声明里有、引擎里还没登记）
		stat := core.SiteStat{
			Key:        key,
			BaseURL:    site.BaseURL,
			StageCount: e.stageCountOfSite(key),
		}
		row, exists := existing[key]
		switch {
		case !exists:
			created := models.CrawlerSite{
				Key: key, BaseURL: stat.BaseURL, StageCount: stat.StageCount,
			}
			// 两个进程同时首启时，唯一键冲突就跳过（另一进程已经建好了），不必报错
			if err := e.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&created).Error; err != nil {
				e.loggerSet.Engine.Errorf("create crawler_sites row %q: %s", key, err.Error())
			}
		default:
			if err := siteKeyScope(e.db, key).
				Updates(map[string]any{"base_url": stat.BaseURL, "stage_count": stat.StageCount}).Error; err != nil {
				e.loggerSet.Engine.Warnf("refresh crawler_sites row %q: %s", key, err.Error())
			}
		}
		// breaker_paused **不跨重启**：熔断闸门每次启动都是新建的、未暂停（暂停状态只活在内存里），
		// 所以这一对列一律按"没暂停"算。库里残留的 true 还要**清掉**，否则后台横幅与「站点」表会
		// 一直显示"熔断已闸住"，而点「恢复」时闸门只会回"当前不在暂停态" —— 那个 true 自己永远
		// 清不掉（只有"先暂停再恢复"两下才能消）。写法与 ResumeSite 一致：解闸就把这一对一起清。
		if row.BreakerPaused {
			if err := siteKeyScope(e.db, key).Updates(map[string]any{
				"breaker_paused": false, "breaker_paused_at": nil,
			}).Error; err != nil {
				e.loggerSet.Engine.Warnf("clear stale breaker_paused for %q: %s", key, err.Error())
			}
		}
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
	if err := siteKeyScope(e.db, site).Updates(updates).Error; err != nil {
		e.loggerSet.Engine.Warnf("save crawler_sites breaker state for %q: %s", site, err.Error())
	}
}
