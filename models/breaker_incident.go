package models

import (
	"gorm.io/datatypes"
	"time"
)

// BreakerIncident is the durable event/report, not the task failure counter.
type BreakerIncident struct {
	ID          string         `gorm:"type:varchar(128);primaryKey;comment:熔断事件ID"`
	Site        string         `gorm:"type:varchar(128);not null;index:idx_breaker_site_time,priority:1;comment:站点"`
	Stage       string         `gorm:"type:varchar(128);not null;default:'';comment:阶段；空为整站"`
	State       string         `gorm:"type:varchar(32);not null;comment:观察冷却试探恢复或人工处理状态"`
	Revision    uint64         `gorm:"not null;comment:事件快照单调版本"`
	Active      bool           `gorm:"not null;index:idx_breaker_active;comment:需要重启恢复保护；人工处理仍为真"`
	NextProbeAt *time.Time     `gorm:"type:datetime(3);serializer:papa_utc;comment:下次试探UTC"`
	StartedAt   time.Time      `gorm:"type:datetime(3);serializer:papa_utc;index:idx_breaker_site_time,priority:2;comment:事件开始UTC"`
	UpdatedAt   time.Time      `gorm:"type:datetime(3);serializer:papa_utc;autoUpdateTime:false;comment:快照更新UTC"`
	Payload     datatypes.JSON `gorm:"type:json;not null;comment:有界脱敏事件报告与恢复进度"`
}

func (BreakerIncident) TableName() string { return "crawler_breaker_incidents" }
