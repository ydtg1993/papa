package models

import "time"

// CrawlerSite 站点表：站点声明与运行统计的落库形态（后台顶部站点 Tab 与「站点」页读它）。
//
// **不落 Headers**：那一栏里有 UA/Cookie 这类凭据，落库等于多一处泄漏面（它是抓取用的，不是给人看的）。
// `BaseURL` / `StageCount` 都是**启动时按声明抄的**（声明只播种），改声明要重启、直接改库不生效 ——
// 页面与文档都要写明这一点。
//
// 与"每站每阶段的池子统计"分工：那些是**实时**数字，走内存快照（`core.StageStats.Site`）不落库；
// 这里只放站点是谁 + 攒下来的慢变统计。
// **列名 `key` 是 MySQL 保留字**：裸写进 SQL 会被当成关键字、整条语句报 Error 1064（真库实测），
// 所以 raw SQL 里一律写 “ `key` “（见 engine/sitestat.go 的 siteKeyScope 与 engine/sqlshape_test.go）。
type CrawlerSite struct {
	ID         uint   `gorm:"primaryKey;comment:站点行 ID"`
	Key        string `gorm:"type:varchar(128);not null;uniqueIndex:ux_site_key;comment:站点键（站点声明的 Key，必填；没有未归属那一档）"`
	BaseURL    string `gorm:"type:varchar(1024);comment:站点根地址（启动时抄的）"`
	StageCount int    `gorm:"not null;default:0;comment:阶段数"`
	// 熔断状态（暂停/恢复那一刻写）
	BreakerPaused   bool       `gorm:"not null;default:false;comment:熔断是否把本站闸住"`
	BreakerPausedAt *time.Time `gorm:"comment:闸住的时间"`
	CreatedAt       time.Time  `gorm:"autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"autoUpdateTime"`
}

// TableName 指定表名，与其它模型保持 crawler_ 前缀。
func (CrawlerSite) TableName() string { return "crawler_sites" }
