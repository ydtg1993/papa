// Package rulequality records value-free runtime quality and connects explicit
// investigations to controlled analysis. Observation coverage is never a claim
// that every production attempt was recorded or that field values are correct.
package rulequality

import (
	"context"
	"errors"
	"time"
)

const (
	MaxObservations       = 10000
	MaxFields             = 64
	MaxReferences         = 64
	MaxPayloadBytes       = 256 << 10
	MaxResultBytes        = 64 << 10
	Pending               = "pending"
	Running               = "running"
	AwaitingReview        = "awaiting_review"
	NeedsEvidence         = "needs_evidence"
	NeedsFixtures         = "needs_fixtures"
	Failed                = "failed"
	Canceled              = "canceled"
	Interrupted           = "interrupted"
	Dismissed             = "dismissed"
	Insufficient          = "insufficient"
	Partial               = "partial"
	ObservedOK            = "observed_ok"
	SuspectedDrift        = "suspected_rule_drift"
	UpstreamFailure       = "upstream_failure"
	ProcessingFailure     = "processing_failure"
	FailureNetwork        = "network"
	FailureHTTP           = "http"
	FailureRateLimit      = "rate_limit"
	FailureAuthentication = "authentication"
	FailureChallenge      = "challenge"
	FailureRender         = "render"
	FailureRule           = "rule_contract"
	FailureExtraction     = "extraction"
	FailureValidation     = "validation"
	FailureBudget         = "budget"
	FailureSave           = "save"
	FailureSubmit         = "submit"
	FailurePlanning       = "planning"
	FailureCanceled       = "canceled"
	FailureHost           = "host"
)

var (
	ErrInvalid       = errors.New("rulequality: invalid contract")
	ErrConflict      = errors.New("rulequality: immutable identity, revision or state conflict")
	ErrNotFound      = errors.New("rulequality: record not found")
	ErrCorrupt       = errors.New("rulequality: persisted content failed validation")
	ErrIncompatible  = errors.New("rulequality: incompatible report baseline")
	ErrEvidence      = errors.New("rulequality: approved evidence unavailable")
	ErrNotActionable = errors.New("rulequality: report does not warrant rule reanalysis")
	ErrUnconfirmed   = errors.New("rulequality: write unconfirmed; inspect request before recovery")
)

type Scope struct {
	Site              string `json:"site"`
	Stage             string `json:"stage"`
	RuleID            string `json:"rule_id"`
	Version           string `json:"version"`
	ReleaseID         string `json:"release_id,omitempty"`
	ReleaseGeneration uint64 `json:"release_generation,omitempty"`
}

// Counts cover RawRows, before deduplication. Invalid counts only known issues;
// incomplete source reports set FieldsIncomplete rather than inventing zeros.
type FieldStat struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Rows     int    `json:"rows"` // denominator for this field, including aggregate reports
	Present  int    `json:"present"`
	Null     int    `json:"null"`
	NonEmpty int    `json:"non_empty"`
	Invalid  int    `json:"invalid"`
}

type Observation struct {
	ID                string      `json:"id"`
	Sequence          uint64      `json:"sequence"`
	CreatedAt         time.Time   `json:"created_at"`
	Scope             Scope       `json:"scope"`
	TaskID            int         `json:"task_id,omitempty"`
	Repeat            int         `json:"repeat,omitempty"`
	Reprocess         int         `json:"reprocess,omitempty"`
	Attempt           int         `json:"attempt,omitempty"`
	LogicalKey        string      `json:"logical_key,omitempty"` // host hash, never raw request/credentials
	RequestSHA256     string      `json:"request_sha256"`
	FieldSchemaSHA256 string      `json:"field_schema_sha256,omitempty"`
	EvidenceID        string      `json:"evidence_id,omitempty"`
	BodySHA256        string      `json:"body_sha256,omitempty"`
	CompleteResponse  bool        `json:"complete_response"`
	HTTPStatus        int         `json:"http_status"`
	Rendered          bool        `json:"rendered"`
	Evaluated         bool        `json:"evaluated"`
	ExtractionOK      bool        `json:"extraction_ok"`
	RawRows           int         `json:"raw_rows"`
	NewRows           int         `json:"new_rows"`
	InvalidRows       int         `json:"invalid_rows"`
	Fields            []FieldStat `json:"fields,omitempty"`
	FieldsIncomplete  bool        `json:"fields_incomplete"`
	Truncated         bool        `json:"truncated"`
	HasNext           bool        `json:"has_next"`
	Page              int         `json:"page"`
	StopReason        string      `json:"stop_reason,omitempty"`
	Operation         string      `json:"operation"`
	FailureKind       string      `json:"failure_kind,omitempty"`
	PlannedChildren   int         `json:"planned_children"`
	SubmittedChildren int         `json:"submitted_children"`
}

type Query struct {
	Scope Scope     `json:"scope"`
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
	Limit int       `json:"limit"`
	// LostObservations is supplied from the collector's health snapshot.
	// Zero does not certify coverage before this process or outside this collector.
	LostObservations uint64 `json:"lost_observations"`
}

type Window struct {
	Query        Query         `json:"query"`
	Cutoff       uint64        `json:"cutoff"`
	Observations []Observation `json:"observations"`
	Truncated    bool          `json:"truncated"`
}

// Nil thresholds are disabled. DefaultPolicy explicitly selects useful initial
// values. Ratios are fractions in [0,1]; sample minima must be positive.
type Policy struct {
	MinPages              int      `json:"min_pages"`
	MinRequests           int      `json:"min_requests"`
	ExtractionFailureRate *float64 `json:"extraction_failure_rate,omitempty"`
	MissingRate           *float64 `json:"missing_rate,omitempty"`
	EmptyRate             *float64 `json:"empty_rate,omitempty"`
	MinRows               *int     `json:"min_rows,omitempty"`
	MaxRowsDrop           *float64 `json:"max_rows_drop,omitempty"`
	MaxFieldDrop          *float64 `json:"max_field_drop,omitempty"`
	CompareAcrossVersions bool     `json:"compare_across_versions"`
}

type Metrics struct {
	Attempts           int            `json:"attempts"`
	Pages              int            `json:"pages"`
	DistinctRequests   int            `json:"distinct_requests"`
	EvaluatedPages     int            `json:"evaluated_pages"`
	ExtractionFailures int            `json:"extraction_failures"`
	RawRows            int            `json:"raw_rows"`
	NewRows            int            `json:"new_rows"`
	InvalidRows        int            `json:"invalid_rows"`
	Fields             []FieldStat    `json:"fields"`
	FailureCounts      map[string]int `json:"failure_counts"`
}

type Signal struct {
	Code  string `json:"code"`
	Field string `json:"field,omitempty"`
	Count int    `json:"count,omitempty"`
}

type EvidenceRef struct {
	ObservationID string `json:"observation_id"`
	EvidenceID    string `json:"evidence_id,omitempty"`
	BodySHA256    string `json:"body_sha256,omitempty"`
}

type Report struct {
	ID                string        `json:"id"`
	CreatedAt         time.Time     `json:"created_at"`
	Query             Query         `json:"query"`
	Cutoff            uint64        `json:"cutoff"`
	InputSHA256       string        `json:"input_sha256"`
	FieldSchemaSHA256 string        `json:"field_schema_sha256,omitempty"`
	Policy            Policy        `json:"policy"`
	BaselineID        string        `json:"baseline_id,omitempty"`
	Coverage          string        `json:"coverage"` // always recorded_observations_only
	Folding           string        `json:"folding"`  // latest_sequence_per_logical_key_or_observation_id
	Truncated         bool          `json:"truncated"`
	Metrics           Metrics       `json:"metrics"`
	Verdict           string        `json:"verdict"`
	Signals           []Signal      `json:"signals,omitempty"`
	Evidence          []EvidenceRef `json:"evidence,omitempty"`
}

type Request struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	ReportID    string    `json:"report_id"`
	RunID       string    `json:"run_id"`
	Operator    string    `json:"operator"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	Revision    uint64    `json:"revision"`
	InputSHA256 string    `json:"input_sha256,omitempty"`
	ResultJSON  string    `json:"result_json"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Recorder interface {
	Record(context.Context, Observation) (Observation, error)
}
type Store interface {
	Recorder
	Query(context.Context, Query) (Window, error)
	SaveReport(context.Context, Report) (Report, error)
	Report(context.Context, string) (Report, error)
	CreateRequest(context.Context, Request) (Request, error)
	Request(context.Context, string) (Request, error)
	RequestByOperation(context.Context, string) (Request, error)
	StartRequest(context.Context, string, uint64, string) (Request, error)
	FinishRequest(context.Context, string, uint64, string, string) (Request, error)
	PruneObservations(context.Context, time.Time, int) (int64, error)
}
