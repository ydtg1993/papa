package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

// ReanalysisRules contains no publication or task-write capability.
type ReanalysisRules interface {
	Get(context.Context, rulestore.Reference) (rulestore.Version, error)
	Snapshot(context.Context, string) (crawlrule.ReleaseSnapshot, error)
	AddSample(context.Context, rulestore.SampleInput, htmlfetch.SamplePolicy, rulestore.Actor) (rulestore.Sample, error)
}

type ReanalysisConfig struct {
	Store Store
	Agent *crawlagent.Agent
	// Audit must be the same authoritative store configured in Agent.
	Audit        agentstore.Store
	Rules        ReanalysisRules
	ArchiveDir   string
	SamplePolicy htmlfetch.SamplePolicy
	Actor        rulestore.Actor
}

type Reanalyzer struct{ config ReanalysisConfig }

type ReanalysisInput struct {
	Goal        crawlagent.Goal `json:"goal"`
	EvidenceIDs []string        `json:"evidence_ids"`
}

type CapabilityGap struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type ReanalysisOutcome struct {
	Request   Request           `json:"request"`
	Analysis  crawlagent.Result `json:"analysis"`
	SampleIDs []string          `json:"sample_ids,omitempty"`
	Gaps      []CapabilityGap   `json:"gaps,omitempty"`
}

func NewReanalyzer(cfg ReanalysisConfig) (*Reanalyzer, error) {
	if cfg.Store == nil || cfg.Agent == nil || cfg.Audit == nil || cfg.Rules == nil || cfg.ArchiveDir == "" || cfg.SamplePolicy.RedactBody == nil || cfg.Actor.Operator == "" || cfg.Actor.Source == "" {
		return nil, ErrInvalid
	}
	cfg.SamplePolicy.QueryKeys = append([]string(nil), cfg.SamplePolicy.QueryKeys...)
	cfg.SamplePolicy.Headers = append([]string(nil), cfg.SamplePolicy.Headers...)
	return &Reanalyzer{config: cfg}, nil
}

// CapabilityGaps reports supported diagnoses, never invents or enables tools.
// Render/network failures alone do not prove that clicking or login is needed.
func CapabilityGaps(report Report) []CapabilityGap {
	out := []CapabilityGap{}
	counts := report.Metrics.FailureCounts
	if counts[FailureAuthentication] > 0 {
		out = append(out, CapabilityGap{"needs_authentication", FailureAuthentication})
	}
	if counts[FailureChallenge] > 0 {
		out = append(out, CapabilityGap{"manual_investigation", FailureChallenge})
	}
	if counts[FailureBudget] > 0 {
		out = append(out, CapabilityGap{"budget_exhausted", FailureBudget})
	}
	if report.Verdict == Insufficient || report.Verdict == Partial {
		out = append(out, CapabilityGap{"insufficient_evidence", report.Verdict})
	}
	if len(out) == 0 && report.Verdict != ObservedOK && report.Verdict != SuspectedDrift {
		out = append(out, CapabilityGap{"manual_investigation", report.Verdict})
	}
	return out
}

// Prepare persists an explicit user operation. A duplicate operation returns its
// original request/RunID; creating a request never invokes the model.
func (r *Reanalyzer) Prepare(ctx context.Context, reportID, operationID string) (Request, error) {
	report, err := r.config.Store.Report(ctx, reportID)
	if err != nil {
		return Request{}, err
	}
	if report.Verdict != SuspectedDrift {
		return Request{}, ErrNotActionable
	}
	return r.config.Store.CreateRequest(ctx, Request{ID: "recheck-" + uuid.NewString(), OperationID: operationID, ReportID: reportID, RunID: "agent-recheck-" + uuid.NewString(), Operator: r.config.Actor.Operator, Source: r.config.Actor.Source})
}

func inputDigest(input ReanalysisInput) (ReanalysisInput, string, error) {
	data, err := json.Marshal(input)
	if err != nil || len(data) > 64<<10 {
		return ReanalysisInput{}, "", ErrInvalid
	}
	var copy ReanalysisInput
	if json.Unmarshal(data, &copy) != nil || len(copy.EvidenceIDs) > 8 || len(copy.Goal.Context) > 2048 {
		return copy, "", ErrInvalid
	}
	sort.Strings(copy.EvidenceIDs)
	for i, id := range copy.EvidenceIDs {
		if len(id) != 32 || i > 0 && copy.EvidenceIDs[i-1] == id {
			return copy, "", ErrInvalid
		}
		if _, err := hex.DecodeString(id); err != nil {
			return copy, "", ErrInvalid
		}
	}
	data, _ = json.Marshal(copy)
	sum := sha256.Sum256(data)
	return copy, hex.EncodeToString(sum[:]), nil
}

func (r *Reanalyzer) checkGoal(ctx context.Context, report Report, goal crawlagent.Goal) error {
	s := report.Query.Scope
	if goal.Site != s.Site || len(goal.Stages) != 1 || len(goal.EvidenceSamples) != 0 {
		return ErrInvalid
	}
	stage := goal.Stages[0]
	if stage.Stage != s.Stage || stage.RuleID != s.RuleID || stage.Existing == nil || stage.Existing.ID != s.RuleID || stage.Existing.Version != s.Version {
		return ErrInvalid
	}
	old, err := r.config.Rules.Get(ctx, *stage.Existing)
	if err != nil || !old.EverPublished {
		return ErrNotActionable
	}
	current, err := r.config.Rules.Snapshot(ctx, s.Site)
	if err != nil {
		return ErrNotActionable
	}
	if current.Bindings[s.Stage] != *stage.Existing || s.ReleaseID != "" && (current.ID != s.ReleaseID || current.Generation != s.ReleaseGeneration) {
		return ErrNotActionable
	}
	if len(stage.Fields) != len(old.Rule.Fields) {
		return ErrInvalid
	}
	fields := map[string]crawlrule.Field{}
	for _, f := range old.Rule.Fields {
		fields[f.Name] = f
	}
	for _, f := range stage.Fields {
		oldField, ok := fields[f.Name]
		if !ok || f.Kind != summaryKind(old.Rule.Format, oldField) || f.Required != oldField.Validation.Required {
			return ErrInvalid
		}
		delete(fields, f.Name)
	}
	return nil
}

// Execute owns one pending request after StartRequest commits. It never takes
// over a running request, and a successful read after a failed CAS is not a lease.
func (r *Reanalyzer) Execute(ctx context.Context, id string, revision uint64, input ReanalysisInput) (out ReanalysisOutcome, err error) {
	if ctx == nil || ctx.Err() != nil {
		return out, ErrInvalid
	}
	request, err := r.config.Store.Request(ctx, id)
	if err != nil {
		return out, err
	}
	out.Request = request
	if request.Status != Pending || request.Revision != revision {
		return out, ErrConflict
	}
	if request.Operator != r.config.Actor.Operator || request.Source != r.config.Actor.Source {
		return out, ErrInvalid
	}
	report, err := r.config.Store.Report(ctx, request.ReportID)
	if err != nil {
		return out, err
	}
	if report.Verdict != SuspectedDrift {
		return out, ErrNotActionable
	}
	input, digest, err := inputDigest(input)
	if err != nil {
		return out, err
	}
	if err = r.checkGoal(ctx, report, input.Goal); err != nil {
		return out, err
	}
	allowed := map[string]string{}
	for _, ref := range report.Evidence {
		if ref.EvidenceID != "" {
			if old, exists := allowed[ref.EvidenceID]; exists && old != ref.BodySHA256 {
				return out, ErrCorrupt
			}
			allowed[ref.EvidenceID] = ref.BodySHA256
		}
	}
	for _, id := range input.EvidenceIDs {
		if allowed[id] == "" {
			return out, ErrEvidence
		}
	}
	request, err = r.config.Store.StartRequest(ctx, id, revision, digest)
	if err != nil {
		return out, ErrUnconfirmed
	}
	out.Request = request
	// A panic in a trusted policy/adapter must not leave this execution authorized
	// to silently run again. Preserve a terminal record when possible.
	defer func() {
		if recover() != nil {
			out, err = r.finish(out, Failed, "host_callback_failed", ErrInvalid)
		}
	}()
	if len(input.EvidenceIDs) == 0 {
		out.Gaps = []CapabilityGap{{"insufficient_evidence", "retained_snapshot_required"}}
		return r.finish(out, NeedsEvidence, "retained_snapshot_required", ErrEvidence)
	}
	for _, evidenceID := range input.EvidenceIDs {
		body, readErr := rulestore.ReadSnapshotMatching(ctx, r.config.ArchiveDir, evidenceID, allowed[evidenceID])
		if readErr != nil {
			if ctx.Err() != nil {
				return r.finish(out, Canceled, "canceled", ctx.Err())
			}
			return r.finish(out, NeedsEvidence, "snapshot_unavailable", ErrEvidence)
		}
		sample, saveErr := r.config.Rules.AddSample(ctx, body, r.config.SamplePolicy, r.config.Actor)
		if saveErr != nil {
			if ctx.Err() != nil {
				return r.finish(out, Canceled, "canceled", ctx.Err())
			}
			return r.finish(out, NeedsEvidence, "sample_not_approved", ErrEvidence)
		}
		out.SampleIDs = append(out.SampleIDs, sample.ID)
	}
	input.Goal.EvidenceSamples = append([]string(nil), out.SampleIDs...)
	signals := report.Signals
	if len(signals) > 8 {
		signals = signals[:8]
	}
	var facts []byte
	for {
		facts, _ = json.Marshal(struct {
			Verdict        string   `json:"verdict"`
			Pages          int      `json:"pages"`
			Rows           int      `json:"rows"`
			Signals        []Signal `json:"signals"`
			SignalsOmitted int      `json:"signals_omitted"`
		}{report.Verdict, report.Metrics.Pages, report.Metrics.RawRows, signals, len(report.Signals) - len(signals)})
		if len(facts) <= 1800 {
			break
		}
		if len(signals) == 0 {
			return r.finish(out, Failed, "feedback_over_budget", ErrInvalid)
		}
		signals = signals[:len(signals)-1]
	}
	input.Goal.Context += "\nRuntime quality feedback (observed samples only): " + string(facts)
	if len(input.Goal.Context) > 4096 {
		return r.finish(out, Failed, "feedback_over_budget", ErrInvalid)
	}
	if ctx.Err() != nil {
		return r.finish(out, Canceled, "canceled", ctx.Err())
	}
	out.Analysis, err = r.config.Agent.RunWithID(ctx, request.RunID, input.Goal)
	if errors.Is(err, crawlagent.ErrAudit) || out.Analysis.Status == agentstore.Interrupted {
		return out, ErrUnconfirmed
	}
	status := analysisStatus(out.Analysis.Status)
	if out.Analysis.Status == agentstore.Exhausted {
		out.Gaps = append(out.Gaps, CapabilityGap{"budget_exhausted", out.Analysis.Reason})
	}
	if status == NeedsFixtures {
		out.Gaps = append(out.Gaps, CapabilityGap{"insufficient_evidence", "independent_fixtures_required"})
	}
	return r.finish(out, status, out.Analysis.Reason, err)
}

func analysisStatus(status string) string {
	switch status {
	case agentstore.AwaitingReview:
		return AwaitingReview
	case agentstore.NeedsFixtures:
		return NeedsFixtures
	case agentstore.Canceled:
		return Canceled
	case agentstore.Interrupted:
		return Interrupted
	default:
		return Failed
	}
}

func (r *Reanalyzer) finish(out ReanalysisOutcome, status, reason string, cause error) (ReanalysisOutcome, error) {
	// No URLs, extracted values, full rule diffs or vendor messages in this table.
	type candidateRef struct {
		Stage            string              `json:"stage"`
		Reference        rulestore.Reference `json:"reference"`
		TrainingReportID string              `json:"training_report_id,omitempty"`
		HoldoutReportID  string              `json:"holdout_report_id,omitempty"`
	}
	refs := []candidateRef{}
	for _, c := range out.Analysis.Candidates {
		refs = append(refs, candidateRef{c.Stage, c.Reference, c.TrainingReportID, c.HoldoutReportID})
	}
	data, marshalErr := json.Marshal(struct {
		RunID      string          `json:"run_id"`
		Verified   bool            `json:"verified"`
		Reason     string          `json:"reason"`
		Candidates []candidateRef  `json:"candidates"`
		SampleIDs  []string        `json:"sample_ids"`
		Gaps       []CapabilityGap `json:"gaps"`
	}{out.Request.RunID, out.Analysis.Verified, reason, refs, out.SampleIDs, out.Gaps})
	if marshalErr != nil || len(data) > MaxResultBytes {
		return out, ErrUnconfirmed
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := r.config.Store.FinishRequest(cleanup, out.Request.ID, out.Request.Revision, status, string(data))
	if err != nil {
		return out, ErrUnconfirmed
	}
	out.Request = request
	return out, cause
}

// Reconcile is read/interrupt only and requires explicit confirmation that the
// old executor ended. It never calls Agent.Run or retries a model/tool request.
func (r *Reanalyzer) Reconcile(ctx context.Context, id string, revision uint64, workerStopped bool) (ReanalysisOutcome, error) {
	out := ReanalysisOutcome{}
	if !workerStopped {
		return out, ErrInvalid
	}
	request, err := r.config.Store.Request(ctx, id)
	if err != nil {
		return out, err
	}
	out.Request = request
	if request.Status != Running || request.Revision != revision {
		return out, ErrConflict
	}
	if request.Operator != r.config.Actor.Operator || request.Source != r.config.Actor.Source {
		return out, ErrInvalid
	}
	run, err := r.config.Audit.Get(ctx, request.RunID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return r.finish(out, Interrupted, "agent_not_started_or_unconfirmed", nil)
	}
	if err != nil {
		return out, ErrUnconfirmed
	}
	reason := "reconciled_from_agent_audit"
	if run.Status == agentstore.Running {
		if run, err = r.config.Audit.Interrupt(ctx, run.ID, run.Revision); err != nil {
			return out, ErrUnconfirmed
		}
		// An interrupted run still owns its durable input snapshot. Recover
		// imported sample references just as after a lost Interrupt response.
		reason = "executor_stopped"
	}
	if len(run.ResultJSON) > agentstore.MaxJSONBytes || json.Unmarshal([]byte(run.ResultJSON), &out.Analysis) != nil {
		return out, ErrCorrupt
	}
	if out.Analysis.RunID != "" && out.Analysis.RunID != request.RunID {
		return out, ErrCorrupt
	}
	if out.Analysis.Status != "" && out.Analysis.Status != run.Status {
		return out, ErrCorrupt
	}
	out.Analysis.RunID = request.RunID
	out.Analysis.Status = run.Status
	// Imported samples were fixed in the durable Agent snapshot before its first
	// model call. Keep those references when only the request finish was lost.
	var snapshot struct{ Goal crawlagent.Goal }
	if len(run.SnapshotJSON) > agentstore.MaxSnapshotBytes || json.Unmarshal([]byte(run.SnapshotJSON), &snapshot) != nil || len(snapshot.Goal.EvidenceSamples) > 64 {
		return out, ErrCorrupt
	}
	out.SampleIDs = append([]string(nil), snapshot.Goal.EvidenceSamples...)
	if run.Status == agentstore.Exhausted {
		out.Gaps = append(out.Gaps, CapabilityGap{"budget_exhausted", out.Analysis.Reason})
	}
	if run.Status == agentstore.NeedsFixtures {
		out.Gaps = append(out.Gaps, CapabilityGap{"insufficient_evidence", "independent_fixtures_required"})
	}
	return r.finish(out, analysisStatus(run.Status), reason, nil)
}
