package models

import "time"

// Durable rule metadata is opt-in. Bodies live in the separate sample blob
// directory; none of these tables retain request credentials or request bodies.
// Binary identifier columns preserve the case-sensitive Go API contract.
type RuleVersion struct {
	RuleID         string    `gorm:"type:varbinary(128);primaryKey;comment:Durable rule RuleID"`
	Version        string    `gorm:"type:varbinary(128);primaryKey;comment:Durable rule Version"`
	Digest         string    `gorm:"type:char(64);not null;comment:Durable rule Digest"`
	ContentJSON    string    `gorm:"type:longtext;not null;comment:Durable rule ContentJSON"`
	State          string    `gorm:"type:varchar(16);not null;comment:Durable rule State"`
	EverPublished  bool      `gorm:"not null;default:false;comment:Durable rule EverPublished"`
	VerificationID string    `gorm:"type:varbinary(64);not null;default:'';comment:Durable rule VerificationID"`
	Operator       string    `gorm:"type:varchar(128);not null;comment:Durable rule Operator"`
	Source         string    `gorm:"type:varchar(256);not null;comment:Durable rule Source"`
	CreatedAt      time.Time `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Durable rule CreatedAt"`
}

func (RuleVersion) TableName() string { return "crawler_rule_versions" }

type RuleSample struct {
	SampleID     string    `gorm:"type:varbinary(64);primaryKey;comment:Durable rule SampleID"`
	Digest       string    `gorm:"type:char(64);not null;comment:Durable rule Digest"`
	MetadataJSON string    `gorm:"type:longtext;not null;comment:Durable rule MetadataJSON"`
	Operator     string    `gorm:"type:varchar(128);not null;comment:Durable rule Operator"`
	Source       string    `gorm:"type:varchar(256);not null;comment:Durable rule Source"`
	CreatedAt    time.Time `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Durable rule CreatedAt"`
}

func (RuleSample) TableName() string { return "crawler_rule_samples" }

type RuleSampleSet struct {
	SetID        string    `gorm:"type:varbinary(128);primaryKey;comment:Durable rule SetID"`
	Digest       string    `gorm:"type:char(64);not null;comment:Durable rule Digest"`
	FixturesJSON string    `gorm:"type:longtext;not null;comment:Durable rule FixturesJSON"`
	Operator     string    `gorm:"type:varchar(128);not null;comment:Durable rule Operator"`
	Source       string    `gorm:"type:varchar(256);not null;comment:Durable rule Source"`
	CreatedAt    time.Time `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Durable rule CreatedAt"`
}

func (RuleSampleSet) TableName() string { return "crawler_rule_sample_sets" }

type RuleVerification struct {
	ID              string    `gorm:"type:varbinary(64);primaryKey;comment:Durable rule ID"`
	RuleID          string    `gorm:"type:varbinary(128);not null;index:idx_rule_verification,priority:1;comment:Durable rule RuleID"`
	Version         string    `gorm:"type:varbinary(128);not null;index:idx_rule_verification,priority:2;comment:Durable rule Version"`
	RuleDigest      string    `gorm:"type:char(64);not null;comment:Durable rule RuleDigest"`
	SetID           string    `gorm:"type:varbinary(128);not null;comment:Durable rule SetID"`
	SetDigest       string    `gorm:"type:char(64);not null;comment:Durable rule SetDigest"`
	VerifierVersion string    `gorm:"type:varchar(64);not null;comment:Durable rule VerifierVersion"`
	Passed          bool      `gorm:"not null;comment:Durable rule Passed"`
	ReportJSON      string    `gorm:"type:longtext;not null;comment:Durable rule ReportJSON"`
	Operator        string    `gorm:"type:varchar(128);not null;comment:Durable rule Operator"`
	Source          string    `gorm:"type:varchar(256);not null;comment:Durable rule Source"`
	CreatedAt       time.Time `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Durable rule CreatedAt"`
}

func (RuleVerification) TableName() string { return "crawler_rule_verifications" }

type RuleRelease struct {
	ID                string    `gorm:"type:varbinary(64);primaryKey;comment:Durable rule ID"`
	OperationID       string    `gorm:"type:varbinary(128);not null;uniqueIndex:uidx_rule_release_operation;comment:Durable rule OperationID"`
	RequestDigest     string    `gorm:"type:char(64);not null;comment:Durable rule RequestDigest"`
	Site              string    `gorm:"type:varbinary(128);not null;uniqueIndex:uidx_rule_release_generation,priority:1;comment:Durable rule Site"`
	Generation        uint64    `gorm:"not null;uniqueIndex:uidx_rule_release_generation,priority:2;comment:Durable rule Generation"`
	BindingsJSON      string    `gorm:"type:longtext;not null;comment:Durable rule BindingsJSON"`
	BindingsDigest    string    `gorm:"type:char(64);not null;comment:Durable rule BindingsDigest"`
	PreviousReleaseID string    `gorm:"type:varbinary(64);not null;default:'';comment:Durable rule PreviousReleaseID"`
	Kind              string    `gorm:"type:varchar(16);not null;comment:Durable rule Kind"`
	Operator          string    `gorm:"type:varchar(128);not null;comment:Durable rule Operator"`
	Source            string    `gorm:"type:varchar(256);not null;comment:Durable rule Source"`
	CreatedAt         time.Time `gorm:"type:datetime(6);not null;default:CURRENT_TIMESTAMP(6);autoCreateTime:false;comment:Durable rule CreatedAt"`
}

func (RuleRelease) TableName() string { return "crawler_rule_releases" }

type RuleSite struct {
	Site             string `gorm:"type:varbinary(128);primaryKey;comment:Durable rule Site"`
	Generation       uint64 `gorm:"not null;default:0;comment:Durable rule Generation"`
	CurrentReleaseID string `gorm:"type:varbinary(64);not null;default:'';comment:Durable rule CurrentReleaseID"`
}

func (RuleSite) TableName() string { return "crawler_rule_sites" }
