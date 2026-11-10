package models

import (
	"time"

	"gorm.io/datatypes"
)

// RuntimePolicyHistory stores immutable, credential-free policy revisions.
// Retention is enforced atomically with policy writes, never by a read request.
type RuntimePolicyHistory struct {
	ConfigKey string         `gorm:"column:cfg_key;type:varchar(64);primaryKey;comment:运行策略键" json:"key"`
	Revision  uint64         `gorm:"primaryKey;autoIncrement:false;comment:策略版本" json:"revision"`
	Document  datatypes.JSON `gorm:"type:json;not null;comment:该版本的后台覆盖配置，不含凭据" json:"document"`
	Operator  string         `gorm:"type:varchar(256);comment:修改操作者" json:"operator"`
	CreatedAt time.Time      `gorm:"comment:策略保存时间" json:"created_at"`
}

func (RuntimePolicyHistory) TableName() string { return "crawler_runtime_policy_history" }
