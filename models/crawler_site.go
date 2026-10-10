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
type CrawlerSite struct {
	ID         uint   `gorm:"primaryKey;comment:站点行 ID"`
	Key        string `gorm:"type:varchar(128);not null;uniqueIndex:ux_site_key;comment:站点键（站点声明的 Key）；空串=默认 scope"`
	BaseURL    string `gorm:"type:varchar(1024);comment:站点根地址（启动时抄的）"`
	AutoRepeat bool   `gorm:"not null;default:true;comment:是否自动轮询（声明只播种，库为事实）"`
	StageCount int    `gorm:"not null;default:0;comment:阶段数"`
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
