package models

import "time"

type TaskStatus int

const (
	TaskStatusPending    TaskStatus = 0 // 待处理
	TaskStatusSuccess    TaskStatus = 1 // 成功
	TaskStatusFailed     TaskStatus = 2 // 失败
	TaskStatusProcessing TaskStatus = 3 // 处理中
)

type CrawlerTask struct {
	ID          uint       `gorm:"primarykey"`
	URL         string     `gorm:"type:varchar(500);not null"`
	Stage       string     `gorm:"type:varchar(50);not null"`
	Retry       int        `gorm:"default:0"`
	Status      TaskStatus `gorm:"default:0;comment:'0:待处理 1:成功 2:失败 3:处理中'"`
	Error       string     `gorm:"type:text"`
	ScheduledAt *time.Time `gorm:"index"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
