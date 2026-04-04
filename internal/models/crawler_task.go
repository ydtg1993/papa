package models

import (
	"time"
)

type TaskStatus int

const (
	TaskStatusPending    TaskStatus = 0 // 待处理
	TaskStatusSuccess    TaskStatus = 1 // 成功
	TaskStatusFailed     TaskStatus = 2 // 失败
	TaskStatusProcessing TaskStatus = 3 // 处理中
)

type CrawlerTask struct {
	ID          uint       `gorm:"primarykey;comment:任务ID"`
	Stage       string     `gorm:"type:varchar(50);not null;uniqueIndex:idx_stage_url,priority:1;comment:所属阶段(catalog/detail等)"`
	URL         string     `gorm:"type:varchar(500);not null;uniqueIndex:idx_stage_url,priority:2;comment:任务URL"`
	Title       string     `gorm:"type:text;comment:页面标题"`
	Content     string     `gorm:"type:longtext;comment:页面HTML内容"`
	Retry       int        `gorm:"default:0;comment:已重试次数"`
	Status      TaskStatus `gorm:"default:0;comment:0:待处理 1:成功 2:失败 3:处理中"`
	Error       string     `gorm:"type:text;comment:错误信息"`
	ScheduledAt *time.Time `gorm:"index;comment:计划执行时间"`
	CreatedAt   time.Time  `gorm:"comment:创建时间"`
	UpdatedAt   time.Time  `gorm:"comment:更新时间"`
}
