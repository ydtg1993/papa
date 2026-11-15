package models

import (
	"gorm.io/datatypes"
	"time"
)

// ConfigOperation makes a configuration write recoverable after a lost response.
type ConfigOperation struct {
	OperationID      string         `gorm:"column:operation_id;type:varchar(128);primaryKey;comment:幂等操作标识"`
	ConfigKey        string         `gorm:"column:cfg_key;type:varchar(128);not null;index;comment:配置域"`
	ExpectedRevision uint64         `gorm:"not null;comment:请求原版本"`
	Revision         uint64         `gorm:"not null;comment:提交版本"`
	Document         datatypes.JSON `gorm:"type:json;not null;comment:提交完整配置"`
	Operator         string         `gorm:"type:varchar(128);not null;comment:认证操作人"`
	Reason           string         `gorm:"type:text;comment:修改原因"`
	CreatedAt        time.Time      `gorm:"index;comment:提交时间"`
}

func (ConfigOperation) TableName() string { return "crawler_config_operations" }
