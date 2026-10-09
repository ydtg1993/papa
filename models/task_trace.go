package models

import (
	"time"

	"gorm.io/datatypes"
)

// TraceStatus 步骤结局。
type TraceStatus int

const (
	TraceOK     TraceStatus = iota // 步骤成功
	TraceFailed                    // 步骤失败
	// TraceWarn 非致命：出了点事，但不该把任务判失败（见 engine.Trace.Warn）。
	// 值追加在末尾 —— 老行里的 0/1 含义不变。
	TraceWarn
)

// TaskTrace 单任务的一次尝试里，一个步骤的记录。
// 由 crawler.trace.enabled 开关控制：开启才建表、才写库。
//
// 每次 FetchHandler 调用是一次「尝试」（attempt），一次尝试里 handler 可以上报多个步骤（seq）。
// Data 只在**失败的尝试**里保留 —— 成功尝试（绝大多数）一条 data 都不写，写入量按失败率走。
type TaskTrace struct {
	ID uint `gorm:"primarykey;comment:记录ID"`

	TaskID  uint `gorm:"index:idx_task_attempt_seq,priority:1;comment:任务ID"`
	Attempt int  `gorm:"index:idx_task_attempt_seq,priority:2;comment:第几次尝试(0起)"`
	Seq     int  `gorm:"index:idx_task_attempt_seq,priority:3;comment:尝试内的步骤序号"`

	Step    string         `gorm:"type:varchar(100);comment:步骤名"`
	Status  TraceStatus    `gorm:"comment:0:成功 1:失败 2:警告(非致命)"`
	Kind    string         `gorm:"type:varchar(50);comment:错误分类(失败/警告时，同 engine.ErrorKind)"`
	Message string         `gorm:"type:text;comment:错误信息(失败/警告时)"`
	Data    datatypes.JSON `gorm:"type:json;comment:该步采集到的数据(仅失败的尝试)"`

	Duration time.Duration `gorm:"comment:该步耗时"`
	// CreatedAt 建索引是为了保留期清理：清理按 created_at 范围删、并按批循环，
	// 没有这个索引每次批量都是一次全表扫描（量越大扫得越多）。
	CreatedAt time.Time `gorm:"autoCreateTime;index;comment:创建时间"`
}

// TableName 指定表名，避免 GORM 复数化规则带来的意外。
func (TaskTrace) TableName() string { return "crawler_task_trace" }
