package models

import "time"

// CrawlerSite 站点表：站点声明与运行统计的落库形态（后台顶部站点 Tab 与「站点」页读它）。
//
// **不落 Headers**：那一栏里有 UA/Cookie 这类凭据，落库等于多一处泄漏面（它是抓取用的，不是给人看的）。
// `BaseURL` / `AutoRepeat` / `StageCount` 都是**启动时按声明抄的**：行已存在就不再动 AutoRepeat
// （声明只播种），改声明要重启、直接改库不生效 —— 页面与文档都要写明这一点。
//
// 与"每站每阶段的池子统计"分工：那些是**实时**数字，走内存快照（`core.StageStats.Site`）不落库；
// 这里只放站点是谁 + 攒下来的慢变统计。
// **列名 `key` 是 MySQL 保留字**：裸写进 SQL 会被当成关键字、整条语句报 Error 1064（真库实测），
// 所以 raw SQL 里一律写 “ `key` “（见 engine/sitestat.go 的 siteKeyScope 与 engine/sqlshape_test.go）。
type CrawlerSite struct {
	ID      uint   `gorm:"primaryKey;comment:站点行 ID"`
	Key     string `gorm:"type:varchar(128);not null;uniqueIndex:ux_site_key;comment:站点键（站点声明的 Key）；空串=默认 scope"`
	BaseURL string `gorm:"type:varchar(1024);comment:站点根地址（启动时抄的）"`
	// **别给这一列加 `default:true`**：gorm 遇到"带默认值的非指针字段 + 零值"时，会把零值
	// **替换成默认值再写回结构体与库** —— 于是站点声明里显式写的 `AutoRepeat: &false`
	// （= "本站只在后台手动触发"）会被静默写成 1，运营第二次启动时"库为事实"就把这一站
	// 自动轮询开起来了，日志里一个字都没有。声明层的"不写 = 自动"是在 SiteSpec.snapshot()
	// 里解析的（那里本来就给 true），不需要靠列默认值兜底。
	AutoRepeat bool `gorm:"not null;comment:是否自动轮询（声明只播种，库为事实）"`
	StageCount int  `gorm:"not null;default:0;comment:阶段数"`
	// 轮询统计（轮询队列每跑完一轮，按列更新自己那几列）
	LastRepeatAt    *time.Time `gorm:"comment:上次轮询队列执行时间"`
	RepeatTotal     int64      `gorm:"not null;default:0;comment:累计轮询重投数"`
	RepeatBacklog   int64      `gorm:"not null;default:0;comment:待轮询（到点未投）条数"`
	LastRepeatError string     `gorm:"type:varchar(512);comment:最近一次轮询队列的错误"`
	// 熔断状态（暂停/恢复那一刻写）
	BreakerPaused   bool       `gorm:"not null;default:false;comment:熔断是否把本站闸住"`
	BreakerPausedAt *time.Time `gorm:"comment:闸住的时间"`
	CreatedAt       time.Time  `gorm:"autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"autoUpdateTime"`
}

// TableName 指定表名，与其它模型保持 crawler_ 前缀。
func (CrawlerSite) TableName() string { return "crawler_sites" }
