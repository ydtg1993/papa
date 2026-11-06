package models

import "time"

// AgentRun retains bounded, sanitized metadata, never website bodies or credentials.
// Sequence bookkeeping is private to agentstore and updated with the revision.
type AgentRun struct {
	ID              string     `gorm:"type:varbinary(128);primaryKey;comment:Agent audit run identifier"`
	Status          string     `gorm:"type:varchar(24);not null;comment:Agent audit run state"`
	Revision        uint64     `gorm:"not null;comment:Agent optimistic revision"`
	SnapshotJSON    string     `gorm:"type:longtext;not null;comment:Sanitized goal policy and budget snapshot"`
	UsageJSON       string     `gorm:"type:mediumtext;not null;comment:Reserved and observed usage metadata"`
	ResultJSON      string     `gorm:"type:mediumtext;not null;comment:Terminal candidate and report references"`
	NextSequence    uint64     `gorm:"not null;default:0;comment:Last allocated step sequence"`
	PendingSequence uint64     `gorm:"not null;default:0;comment:Pending step sequence or zero"`
	CreatedAt       time.Time  `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Database run creation time"`
	UpdatedAt       time.Time  `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoUpdateTime:false;comment:Database run mutation time"`
	FinishedAt      *time.Time `gorm:"type:datetime(6);comment:Database run termination time"`
}

func (AgentRun) TableName() string { return "crawler_agent_runs" }

// AgentStep contains only sanitized action metadata and durable evidence references.
type AgentStep struct {
	RunID      string     `gorm:"type:varbinary(128);primaryKey;comment:Owning agent audit run"`
	Sequence   uint64     `gorm:"primaryKey;autoIncrement:false;comment:Monotonic step sequence within run"`
	Kind       string     `gorm:"type:varchar(64);not null;comment:Restricted action kind"`
	InputJSON  string     `gorm:"type:mediumtext;not null;comment:Sanitized action input metadata"`
	OutputJSON string     `gorm:"type:mediumtext;not null;comment:Sanitized action output references"`
	Status     string     `gorm:"type:varchar(16);not null;comment:Pending or terminal step state"`
	StartedAt  time.Time  `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);comment:Database step start time"`
	FinishedAt *time.Time `gorm:"type:datetime(6);comment:Database step completion time"`
}

func (AgentStep) TableName() string { return "crawler_agent_steps" }
