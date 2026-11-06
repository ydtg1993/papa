package engine

import (
	"context"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// RuleRef pins a fixture-verified rule version. Active is never resolved by a worker.
type RuleRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// RuleRequest contains only replayable request identity, never authentication data.
// State is an optional, non-secret cursor; BuildRequest supplies headers/body at runtime.
type RuleRequest struct {
	URL   string `json:"url"`
	State string `json:"state,omitempty"`
}

type RuleLink struct {
	Field string
	Stage string
	// Meta maps child metadata keys to extracted record fields. Reserved keys are rejected.
	Meta map[string]string
}

// RulePage is accepted data and its provenance. Save may be called again after a
// partial failure and must upsert using business identity, not append blindly.
type RulePage struct {
	// EvidenceID correlates this result with the attempt's snapshot and Trace.
	// It is empty without an archive context; an ID alone does not mean a body
	// was retained (failure mode deliberately omits successful observations).
	EvidenceID string
	// ReleaseID/ReleaseGeneration identify the publication pinned by this task.
	// They are empty/zero for the original static Registry binding workflow.
	ReleaseID         string
	ReleaseGeneration uint64
	Rule              RuleRef
	SourceURL         string
	BodySHA256        string
	StatusCode        int // zero for rendered DOM: no invented HTTP status
	Rendered          bool
	Result            crawlrule.Result
	Page              int
	StopReason        string
}

type RuleFetcherConfig struct {
	Site, Stage string
	Registry    *crawlrule.Registry
	// Catalog enables durable, site-wide publications. It is mutually exclusive
	// with Registry and static Bindings. NewTaskContext reads one full snapshot;
	// running and child tasks continue to use their pinned versions.
	Catalog crawlrule.Catalog
	// Bindings includes this stage and all linked stages. NewTask snapshots them.
	Bindings map[string]RuleRef
	// Origins is an explicit list of allowed HTTP(S) origins, mandatory.
	Origins []string
	// Transport: "http" (HTML or JSON) or "browser" (HTML only).
	Transport    string
	AccountID    string
	WaitSelector string // browser DOM readiness after navigation
	// SnapshotPolicy explicitly permits/sanitizes response snapshots. Nil or a
	// nil RedactBody omits bodies; query parameters are omitted unless allowlisted.
	// Request bodies/headers are never captured. Requires crawler.archive enabled
	// and an Engine attempt context; direct RunHTTP does not create an archive.
	SnapshotPolicy *htmlfetch.SamplePolicy
	// BuildRequest must be deterministic for a RuleRequest and must not change URL.
	// Its credentials are transient. POST requires ReplaySafe because the whole
	// handler, including a successful request followed by failed Save, can repeat.
	BuildRequest func(context.Context, *Task, RuleRequest) (htmlfetch.Request, error)
	ReplaySafe   bool
	// Next overrides Result.NextURL/NextCursor; nil means no next page.
	// Return a canonical URL and optional non-secret State. No raw body is exposed.
	Next               func(context.Context, *Task, RulePage) (*RuleRequest, error)
	Links              []RuleLink
	ItemKey            string // record field used to deduplicate items across pages; empty hashes record
	MaxPages, MaxItems int    // default 100 / 10000, hard ceilings 1000 / 10000
	// MaxChildren bounds all detail and next-page tasks planned in one attempt.
	MaxChildren int // default 1000, hard ceiling 10000; combined metadata also capped at 8 MiB
	Save        func(context.Context, *Task, RulePage) error
}

type RuleFetcher struct{ config RuleFetcherConfig }

const (
	ruleBindingsMeta   = "_rule_bindings"
	ruleRequestMeta    = "_rule_request"
	ruleCheckpointMeta = "_rule_checkpoint"
	ruleAccountMeta    = "_rule_account"
	ruleReleaseMeta    = "_rule_release"
)
