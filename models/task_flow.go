package models

import "gorm.io/datatypes"

// TaskFlow serializes policy updates and task admission within one bounded run.
type TaskFlow struct {
	ID       string         `gorm:"type:varchar(64);primaryKey;comment:调度运行ID" json:"id"`
	Site     string         `gorm:"type:varchar(128);not null;index;comment:所属站点" json:"site"`
	Revision uint64         `gorm:"not null;comment:策略修订" json:"revision"`
	Policy   datatypes.JSON `gorm:"type:json;not null;comment:阶段范围及调度策略" json:"policy"`
}

func (TaskFlow) TableName() string { return "crawler_task_flows" }
