package models

import (
	"gorm.io/datatypes"
	"time"
)

// BreakerHistory is append-only process history. Control still lives in the
// incident snapshot. Sequence makes a lost-acknowledgement retry idempotent.
type BreakerHistory struct {
	IncidentID string         `gorm:"type:varchar(128);primaryKey;comment:所属熔断事件"`
	Sequence   uint64         `gorm:"primaryKey;autoIncrement:false;comment:事件内追加序号"`
	Site       string         `gorm:"type:varchar(128);not null;index:idx_breaker_history_site_time,priority:1;comment:站点"`
	At         time.Time      `gorm:"type:datetime(3);serializer:papa_utc;index:idx_breaker_history_time;index:idx_breaker_history_site_time,priority:2;comment:过程记录发生时间UTC"`
	State      string         `gorm:"type:varchar(32);not null;comment:记录发生后的保护状态"`
	Payload    datatypes.JSON `gorm:"type:json;not null;comment:当时验证范围样本结果及运行过程"`
}

func (BreakerHistory) TableName() string { return "crawler_breaker_history" }
