package models

// RuleSchedule retains the immutable batch root allocated by a repeatable rule
// control task. It is migrated explicitly only when rules are enabled.
type RuleSchedule struct {
	TaskID        uint   `gorm:"primaryKey;autoIncrement:false;comment:Rule schedule control task ID"`
	Revision      uint64 `gorm:"not null;comment:Rule schedule state revision"`
	PayloadSHA256 string `gorm:"type:char(64);not null;comment:Rule schedule payload SHA-256"`
	PayloadJSON   string `gorm:"type:longtext;not null;comment:Rule schedule immutable batch payload"`
}

func (RuleSchedule) TableName() string { return "crawler_rule_schedules" }
