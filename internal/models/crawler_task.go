package models

import (
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"time"
)

type TaskStatus int

const (
	TaskStatusPending    TaskStatus = iota // 待处理
	TaskStatusProcessing                   // 处理中
	TaskStatusSuccess                      // 成功
	TaskStatusFailed                       // 失败
)

type RepeatableStatus int

const (
	RepeatableNo RepeatableStatus = iota
	RepeatableYes
)

type CrawlerTask struct {
	ID         uint             `gorm:"primarykey;comment:任务ID"`
	PID        uint             `gorm:"index;type:int(11);default:0;comment:父级任务ID"`
	Stage      string           `gorm:"type:varchar(50);not null;uniqueIndex:idx_stage_url,priority:2;comment:所属阶段(catalog/detail等)"`
	URL        string           `gorm:"type:varchar(500);not null;uniqueIndex:idx_stage_url,priority:1;comment:任务URL"`
	Title      string           `gorm:"type:text;comment:页面标题"`
	Content    datatypes.JSON   `gorm:"type:json;comment:页面提取内容"`
	Retry      int              `gorm:"default:0;comment:错误重试次数"`
	Status     TaskStatus       `gorm:"default:0;comment:0:待处理 1:处理中 2:成功 3:失败"`
	Repeatable RepeatableStatus `gorm:"type:tinyint(1);default:0;comment:支持重试 0:不能 1:可以"`
	Repeat     int              `gorm:"type:int(11);default:0;comment:轮询重试次数"`
	Error      string           `gorm:"type:text;comment:错误信息"`
	CreatedAt  time.Time        `gorm:"autoCreateTime;comment:创建时间"`
	UpdatedAt  time.Time        `gorm:"autoUpdateTime;comment:更新时间"`
}

func (t *CrawlerTask) BeforeCreate(tx *gorm.DB) error {
	if t.Content == nil || len(t.Content) == 0 {
		t.Content = datatypes.JSON("{}")
	}
	return nil
}
