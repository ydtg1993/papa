package models

import (
	"time"
)

type TaskStatus int

const (
	TaskStatusPending    TaskStatus = iota // 待处理
	TaskStatusProcessing                   // 处理中
	TaskStatusSuccess                      // 成功
	TaskStatusFailed                       // 失败
)

type CrawlerTask struct {
	ID          uint       `gorm:"primarykey;comment:任务ID"`
	PID         uint       `gorm:"index;type:int(11);comment:父级任务ID"`
	Stage       string     `gorm:"type:varchar(50);not null;uniqueIndex:idx_stage_url,priority:1;comment:所属阶段(catalog/detail等)"`
	URL         string     `gorm:"type:varchar(500);not null;uniqueIndex:idx_stage_url,priority:2;comment:任务URL"`
	Title       string     `gorm:"type:text;comment:页面标题"`
	Content     string     `gorm:"type:json;comment:页面提取内容"`
	Retry       int        `gorm:"default:0;comment:已重试次数"`
	Status      TaskStatus `gorm:"default:0;comment:0:待处理 1:处理中 2:成功 3:失败"`
	Error       string     `gorm:"type:text;comment:错误信息"`
	ScheduledAt *time.Time `gorm:"index;comment:计划执行时间"`
	CreatedAt   time.Time  `gorm:"comment:创建时间"`
	UpdatedAt   time.Time  `gorm:"comment:更新时间"`
}
