package models

import "time"

// RuleObservation is an immutable, bounded, value-free quality observation.
type RuleObservation struct {
	Sequence      uint64    `gorm:"primaryKey;autoIncrement;index:idx_quality_scope,priority:3;index:idx_quality_prune,priority:2;comment:Monotonic observation cutoff sequence"`
	ID            string    `gorm:"type:varbinary(128);not null;uniqueIndex;comment:Caller observation identity"`
	ScopeSHA256   string    `gorm:"type:char(64);not null;index:idx_quality_scope,priority:1;comment:Exact site stage rule version and release scope digest"`
	PayloadSHA256 string    `gorm:"type:char(64);not null;comment:Canonical bounded observation payload digest"`
	PayloadJSON   string    `gorm:"type:mediumtext;not null;comment:Value-free immutable observation JSON"`
	CreatedAt     time.Time `gorm:"type:datetime(6);not null;autoCreateTime:false;index:idx_quality_scope,priority:2;index:idx_quality_prune,priority:1;comment:Database observation creation time"`
}

func (RuleObservation) TableName() string { return "crawler_rule_observations" }

// RuleQualityReport freezes input cutoff, policy and aggregates independently of retention.
type RuleQualityReport struct {
	ID            string    `gorm:"type:varbinary(128);primaryKey;comment:Immutable quality report identity"`
	ScopeSHA256   string    `gorm:"type:char(64);not null;index;comment:Exact report scope digest"`
	InputSHA256   string    `gorm:"type:char(64);not null;comment:Frozen report input digest"`
	Cutoff        uint64    `gorm:"not null;comment:Frozen observation sequence cutoff"`
	PayloadSHA256 string    `gorm:"type:char(64);not null;comment:Canonical bounded report payload digest"`
	PayloadJSON   string    `gorm:"type:mediumtext;not null;comment:Immutable quality report JSON without page bodies"`
	CreatedAt     time.Time `gorm:"type:datetime(6);not null;autoCreateTime:false;comment:Database report creation time"`
}

func (RuleQualityReport) TableName() string { return "crawler_rule_quality_reports" }

// RuleReanalysisRequest reserves a single Agent run before any model invocation.
type RuleReanalysisRequest struct {
	ID            string    `gorm:"type:varbinary(128);primaryKey;comment:Reanalysis request identity"`
	OperationID   string    `gorm:"type:varbinary(128);not null;uniqueIndex;comment:Host idempotency operation identity"`
	ReportID      string    `gorm:"type:varbinary(128);not null;index;comment:Immutable originating quality report"`
	RunID         string    `gorm:"type:varbinary(128);not null;uniqueIndex;comment:Preassigned unique Agent run identity"`
	Status        string    `gorm:"type:varchar(24);not null;comment:Pending running or terminal request state"`
	Revision      uint64    `gorm:"not null;comment:Optimistic request revision"`
	PayloadSHA256 string    `gorm:"type:char(64);not null;comment:Bounded request payload integrity digest"`
	PayloadJSON   string    `gorm:"type:mediumtext;not null;comment:Actor input digest and bounded outcome JSON"`
	CreatedAt     time.Time `gorm:"type:datetime(6);not null;autoCreateTime:false;comment:Database request creation time"`
	UpdatedAt     time.Time `gorm:"type:datetime(6);not null;autoUpdateTime:false;comment:Database request update time"`
}

func (RuleReanalysisRequest) TableName() string { return "crawler_rule_reanalysis_requests" }
