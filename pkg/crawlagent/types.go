// Package crawlagent analyzes approved website evidence and proposes immutable
// rules. It never publishes releases or executes production collection tasks.
package crawlagent

import (
	"context"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

var (
	ErrInvalid  = errors.New("crawlagent: invalid host contract")
	ErrAction   = errors.New("crawlagent: invalid or unauthorized action")
	ErrBudget   = errors.New("crawlagent: analysis budget exhausted")
	ErrAudit    = errors.New("crawlagent: audit persistence failed; inspect run before recovery")
	ErrEvidence = errors.New("crawlagent: evidence unavailable or not approved for generation")
)

type FieldGoal struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Required    bool   `json:"required"`
}

type LinkGoal struct {
	Field string            `json:"field"`
	Stage string            `json:"stage"`
	Meta  map[string]string `json:"meta,omitempty"`
}

type StageGoal struct {
	Stage       string               `json:"stage"`
	RuleID      string               `json:"rule_id"`
	Fields      []FieldGoal          `json:"fields"`
	Transport   string               `json:"transport"` // http or browser
	ItemKey     string               `json:"item_key,omitempty"`
	Links       []LinkGoal           `json:"links,omitempty"`
	TrainingSet string               `json:"training_set,omitempty"`
	HoldoutSet  string               `json:"holdout_set,omitempty"`
	Existing    *rulestore.Reference `json:"existing,omitempty"`
}

type Goal struct {
	Site   string      `json:"site"`
	Seeds  []string    `json:"seeds"`
	Stages []StageGoal `json:"stages"`
	// EvidenceSamples are host-selected observations, not fixture expectations.
	// They undergo the same current disclosure policy and holdout isolation.
	EvidenceSamples []string `json:"evidence_samples,omitempty"`
	// Context contains host-approved investigation facts, never added authority.
	Context string `json:"context,omitempty"`
}

// Prices are host-supplied million-token rates in integer micro currency units.
// They are estimates, not a guarantee of an external provider's billing.
type Prices struct {
	InputPerMillionMicros  int64 `json:"input_per_million_micros"`
	OutputPerMillionMicros int64 `json:"output_per_million_micros"`
	MaxMicros              int64 `json:"max_micros"`
}

type Budgets struct {
	MaxDuration           time.Duration
	MaxModelCalls         int
	MaxCandidatesPerStage int
	MaxOutputTokens       int
	MaxInputBytes         int
	MaxTotalInputBytes    int
	Prices                Prices
}

type Usage struct {
	ModelCalls      int   `json:"model_calls"`
	InputBytes      int64 `json:"input_bytes"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	EstimatedMicros int64 `json:"estimated_micros"`
	CostKnown       bool  `json:"cost_known"`
	Requests        int   `json:"requests"`
	Pages           int   `json:"pages"`
	Bytes           int64 `json:"bytes"`
}

// Repository deliberately has no publication or production-task methods.
type Repository interface {
	CreateDraft(context.Context, crawlrule.Rule, rulestore.Actor) (rulestore.Version, error)
	Get(context.Context, rulestore.Reference) (rulestore.Version, error)
	AddSample(context.Context, rulestore.SampleInput, htmlfetch.SamplePolicy, rulestore.Actor) (rulestore.Sample, error)
	Sample(context.Context, string) (rulestore.Sample, error)
	SampleSet(context.Context, string) (rulestore.SampleSet, error)
	Verify(context.Context, rulestore.Reference, string, rulestore.Actor) (rulestore.Verification, error)
}

type BlobReader interface {
	Get(context.Context, string, int) ([]byte, error)
}

type Source interface {
	Get(context.Context, string) (webaccess.Response, error)
	Render(context.Context, string, webaccess.RenderOptions) (webaccess.Response, []webaccess.Response, error)
	Usage() webaccess.Usage
	Close() error
}

type Config struct {
	Model        model.Provider
	ModelName    string
	Audit        agentstore.Store
	Repository   Repository
	Blobs        BlobReader
	Actor        rulestore.Actor
	SamplePolicy htmlfetch.SamplePolicy
	Web          webaccess.Config
	// AllowRecordAPI is separate from Web.Renderer.Enabled and defaults to false.
	AllowRecordAPI bool
	Budgets        Budgets
	// SourceFactory is a trusted host adapter, mainly for deterministic tests.
	// It must enforce Web policy and invoke the supplied accounting callbacks.
	// The model cannot configure or replace it.
	SourceFactory func(webaccess.Config) (Source, error)
}

type Candidate struct {
	Stage            string                    `json:"stage"`
	Reference        rulestore.Reference       `json:"reference"`
	TrainingReportID string                    `json:"training_report_id,omitempty"`
	TrainingPassed   bool                      `json:"training_passed"`
	HoldoutReportID  string                    `json:"holdout_report_id,omitempty"`
	HoldoutPassed    bool                      `json:"holdout_passed"`
	Comparison       *rulestore.RuleComparison `json:"comparison,omitempty"`
	Shadow           []ShadowSummary           `json:"shadow,omitempty"`
}

type ShadowSummary struct {
	EvidenceID string                 `json:"evidence_id"`
	Report     rulestore.ShadowReport `json:"report"`
}

type Evidence struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	BodySHA256  string `json:"body_sha256"`
	BodyBytes   int    `json:"body_bytes"`
	StatusCode  int    `json:"status_code"`
	Rendered    bool   `json:"rendered"`
	ContentType string `json:"content_type,omitempty"`
}

type Result struct {
	RunID      string      `json:"run_id"`
	Status     string      `json:"status"`
	Reason     string      `json:"reason"`
	Verified   bool        `json:"verified"`
	Candidates []Candidate `json:"candidates"`
	Evidence   []Evidence  `json:"evidence"`
	Usage      Usage       `json:"usage"`
}
