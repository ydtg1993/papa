// Package agentadmin connects explicit human review to existing bounded Agent
// execution. It has no publication or production-task mutation capability.
package agentadmin

import (
	"context"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

var (
	ErrInvalid       = errors.New("agentadmin: invalid input")
	ErrDisabled      = errors.New("agentadmin: capability not configured")
	ErrReviewChanged = errors.New("agentadmin: reviewed plan changed")
	ErrBusy          = errors.New("agentadmin: executor busy or closing")
	ErrOwner         = errors.New("agentadmin: operator or profile mismatch")
	ErrStopped       = errors.New("agentadmin: previous executor stop is not established")
)

type Audit interface {
	agentstore.Store
	List(context.Context, agentstore.RunFilter) (agentstore.RunPage, error)
	Inspect(context.Context, string, uint64) (agentstore.RunDetail, error)
}
type Quality interface {
	rulequality.Store
	Requests(context.Context, string, string) (rulequality.RequestPage, error)
}
type Rules interface {
	Inspect(context.Context, rulestore.Reference) (rulestore.VersionDetail, error)
	Verification(context.Context, string) (rulestore.Verification, error)
	Releases(context.Context, string, uint64) (rulestore.ReleasePage, error)
}

// Profile is trusted host code, registered before serving HTTP. Increment
// Revision when goals, disclosure policy, model or capabilities change.
type Profile struct {
	ID, Revision, Label string
	Agent               crawlagent.Config
	ArchiveDir          string
	Goal                func(context.Context, rulequality.Report) (crawlagent.Goal, error)
}
type Config struct {
	Audit         Audit
	Quality       Quality
	Rules         Rules
	Profiles      []Profile
	MaxConcurrent int
	// Required for reconciliation; must establish cessation across ALL hosts.
	// A local map miss, elapsed deadline or browser checkbox is insufficient.
	WorkerStopped func(context.Context, rulequality.Request) bool
}

// Options configures App-owned administration. Storage is supplied by App;
// profiles and the cross-host stop guard are supplied explicitly by its host.
type Options struct {
	Profiles      []Profile
	MaxConcurrent int
	WorkerStopped func(context.Context, rulequality.Request) bool
}
type ProfileView struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Label    string `json:"label"`
}
type Capabilities struct {
	Profiles      []ProfileView `json:"profiles"`
	Reconcile     bool          `json:"reconcile"`
	MaxConcurrent int           `json:"max_concurrent"`
	Quality       bool          `json:"quality"`
	Rules         bool          `json:"rules"`
}
type Selection struct {
	ReportID    string   `json:"report_id"`
	ProfileID   string   `json:"profile_id"`
	EvidenceIDs []string `json:"evidence_ids"`
}
type Preview struct {
	Selection Selection       `json:"selection"`
	Profile   ProfileView     `json:"profile"`
	Digest    string          `json:"digest"`
	Plan      crawlagent.Plan `json:"plan"`
}
type PrepareInput struct {
	Selection
	Digest      string `json:"digest"`
	OperationID string `json:"operation_id"`
}
type StartInput struct {
	Selection
	Digest   string `json:"digest"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type ControlInput struct {
	ID        string `json:"id"`
	Revision  string `json:"revision"`
	Confirmed bool   `json:"confirmed"`
}
type RequestView struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	ReportID    string    `json:"report_id"`
	RunID       string    `json:"run_id"`
	Operator    string    `json:"operator"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	Revision    string    `json:"revision"`
	Active      bool      `json:"active"`
	LocalError  string    `json:"local_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type RequestPage struct {
	Items []RequestView `json:"items"`
	Next  string        `json:"next"`
}
type RunView struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"`
	Revision   string             `json:"revision"`
	Site       string             `json:"site"`
	Model      string             `json:"model"`
	Operator   string             `json:"operator"`
	Usage      crawlagent.Usage   `json:"usage"`
	Budgets    crawlagent.Budgets `json:"budgets"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
}
type RunPage struct {
	Items   []RunView `json:"items"`
	Scanned int       `json:"scanned"`
	Next    string    `json:"next"`
}
type CandidateRef struct {
	Stage            string              `json:"stage"`
	Reference        rulestore.Reference `json:"reference"`
	TrainingReportID string              `json:"training_report_id,omitempty"`
	HoldoutReportID  string              `json:"holdout_report_id,omitempty"`
}
type StepView struct {
	Sequence   string         `json:"sequence"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	Tool       string         `json:"tool,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Candidates []CandidateRef `json:"candidates,omitempty"`
}
type RunDetail struct {
	Run        RunView        `json:"run"`
	Steps      []StepView     `json:"steps"`
	Next       string         `json:"next"`
	Candidates []CandidateRef `json:"candidates"`
	Verified   bool           `json:"verified"`
}
type Review struct {
	RunID       string                     `json:"run_id"`
	Site        string                     `json:"site"`
	Candidate   CandidateRef               `json:"candidate"`
	Version     rulestore.VersionDetail    `json:"version"`
	Training    *rulestore.Verification    `json:"training,omitempty"`
	Holdout     *rulestore.Verification    `json:"holdout,omitempty"`
	Comparison  *rulestore.RuleComparison  `json:"comparison,omitempty"`
	Shadow      []crawlagent.ShadowSummary `json:"shadow,omitempty"`
	Publishable bool                       `json:"publishable"`
	BlockReason string                     `json:"block_reason"`
}
