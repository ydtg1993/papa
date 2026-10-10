package models

import (
	"time"

	"gorm.io/datatypes"
)

// OperationLog 后台 OA 的增删改操作记录。
// 由 server.operation_log 开关控制：开启才建表、才写库。
type OperationLog struct {
	ID uint `gorm:"primarykey" json:"id"`

	Table  string         `gorm:"type:varchar(128);not null;index;comment:表格 key" json:"table"`
	Action string         `gorm:"type:varchar(128);not null;comment:动作 key" json:"action"`
	RowID  string         `gorm:"type:varchar(64);comment:目标行主键" json:"row_id"`
	Values datatypes.JSON `gorm:"type:json;comment:提交的字段值" json:"values"`

	// Operator 谁干的：由鉴权中间件从访问令牌解析出操作人、写进请求上下文，
	// OnAction 回调再从请求里取出来。老数据（无令牌时代）为空。
	Operator string `gorm:"type:varchar(128);index;comment:操作人（来自访问令牌）" json:"operator"`

	OK    bool   `gorm:"not null;default:false;index;comment:是否成功" json:"ok"`
	Error string `gorm:"type:text;comment:失败原因" json:"error"`
	IP    string `gorm:"type:varchar(128);index;comment:来源 IP" json:"ip"`

	CreatedAt time.Time `gorm:"autoCreateTime;index;comment:操作时间" json:"created_at"`
}

// TableName 指定表名，避免 GORM 复数化规则带来的意外。
func (OperationLog) TableName() string { return "crawler_operation_log" }
