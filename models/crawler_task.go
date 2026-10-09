package models

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
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
	ID             uint   `gorm:"primarykey;comment:任务ID"`
	PID            uint   `gorm:"index;type:int(11);default:0;comment:父级任务ID"`
	Stage          string `gorm:"type:varchar(50);not null;index;uniqueIndex:idx_stage_url,priority:2;comment:所属阶段(例如catalog/detail等)"`
	URL            string `gorm:"type:varchar(500);not null;uniqueIndex:idx_stage_url,priority:1;comment:任务URL"`
	IdempotencyKey string `gorm:"type:varchar(500);index;comment:自定义幂等键，空则回退 stage|url"`
	// Meta 业务键（如 series_id/episode_id），与 URL 解耦。**随行落库** ——
	// 恢复/轮询/错误队列/后台重投这几条「从行重建 Task」的路都按它还原任务身份，
	// 不落库的话任务重投一次身份就没了（handler 拿不到业务键，报错还指不到这里）。
	Meta       datatypes.JSON   `gorm:"type:json;comment:业务键(如 series_id/episode_id)，随行落库"`
	Title      string           `gorm:"type:text;comment:页面标题"`
	Content    datatypes.JSON   `gorm:"type:json;comment:页面提取内容"`
	Retry      int              `gorm:"default:0;comment:错误重试次数"`
	Status     TaskStatus       `gorm:"default:0;comment:0:待处理 1:处理中 2:成功 3:失败"`
	Repeatable RepeatableStatus `gorm:"type:tinyint(1);default:0;comment:支持重试 0:不能 1:可以"`
	Repeat     int              `gorm:"type:int(11);default:0;comment:轮询重试次数"`
	Reprocess  int              `gorm:"type:int(11);default:0;comment:失败任务被错误队列再处理的代数"`
	// Urgent 加急：该任务投到所属阶段的快车道，插到常规队列前面。
	// 它是「排队位置」的概念 —— worker 认领（置为处理中）时一并归零，跑过一次即完成使命。
	Urgent    bool      `gorm:"type:tinyint(1);default:0;comment:加急 0:否 1:是"`
	Error     string    `gorm:"type:text;comment:错误信息"`
	CreatedAt time.Time `gorm:"autoCreateTime;comment:创建时间"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;comment:更新时间"`
}

func (t *CrawlerTask) BeforeCreate(tx *gorm.DB) error {
	if t.Content == nil || len(t.Content) == 0 {
		t.Content = datatypes.JSON("{}")
	}
	return nil
}
