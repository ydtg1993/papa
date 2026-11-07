package agentadmin

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

func candidateRef(c crawlagent.Candidate) CandidateRef {
	return CandidateRef{c.Stage, c.Reference, c.TrainingReportID, c.HoldoutReportID}
}
func parseRun(r agentstore.Run) (crawlagent.Plan, crawlagent.Result, RunView, error) {
	var plan crawlagent.Plan
	var result crawlagent.Result
	var usage crawlagent.Usage
	out := RunView{ID: r.ID, Status: r.Status, Revision: strconv.FormatUint(r.Revision, 10), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FinishedAt: r.FinishedAt}
	if json.Unmarshal([]byte(r.SnapshotJSON), &plan) != nil || json.Unmarshal([]byte(r.ResultJSON), &result) != nil || json.Unmarshal([]byte(r.UsageJSON), &usage) != nil || len(result.Candidates) > 16 || len(plan.Goal.Stages) > 16 {
		return plan, result, out, agentstore.ErrCorrupt
	}
	if result.RunID != "" && result.RunID != r.ID || result.Status != "" && result.Status != r.Status {
		return plan, result, out, agentstore.ErrCorrupt
	}
	out.Site, out.Model, out.Operator, out.Budgets, out.Usage = plan.Goal.Site, plan.Model, plan.Actor.Operator, plan.Budgets, usage
	return plan, result, out, nil
}
func (s *Service) Runs(ctx context.Context, f agentstore.RunFilter) (RunPage, error) {
	out := RunPage{Items: []RunView{}}
	p, err := s.cfg.Audit.List(ctx, f)
	if err != nil {
		return out, err
	}
	out.Scanned, out.Next = p.Scanned, p.Next
	for _, run := range p.Items {
		_, _, view, err := parseRun(run)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, view)
	}
	return out, nil
}
func (s *Service) Run(ctx context.Context, id string, after uint64) (RunDetail, error) {
	out := RunDetail{Steps: []StepView{}, Candidates: []CandidateRef{}}
	d, err := s.cfg.Audit.Inspect(ctx, id, after)
	if err != nil {
		return out, err
	}
	_, result, view, err := parseRun(d.Run)
	if err != nil {
		return out, err
	}
	out.Run = view
	out.Next = strconv.FormatUint(d.Next, 10)
	out.Verified = result.Verified
	for _, c := range result.Candidates {
		out.Candidates = append(out.Candidates, candidateRef(c))
	}
	for _, step := range d.Steps {
		var input struct {
			Tool string `json:"tool"`
		}
		var output struct {
			References struct {
				Candidates []CandidateRef `json:"candidates"`
			} `json:"references"`
		}
		if json.Unmarshal([]byte(step.InputJSON), &input) != nil || json.Unmarshal([]byte(step.OutputJSON), &output) != nil || len(output.References.Candidates) > 16 {
			return out, agentstore.ErrCorrupt
		}
		// Only the known tool code and structured references escape the audit store.
		if !code(input.Tool, 64) {
			input.Tool = ""
		}
		out.Steps = append(out.Steps, StepView{strconv.FormatUint(step.Sequence, 10), step.Kind, step.Status, input.Tool, step.StartedAt, step.FinishedAt, output.References.Candidates})
	}
	return out, nil
}
func (s *Service) Review(ctx context.Context, id string, ref rulestore.Reference) (Review, error) {
	out := Review{RunID: id, BlockReason: "independent_verification_required"}
	if s.cfg.Rules == nil {
		return out, ErrDisabled
	}
	run, err := s.cfg.Audit.Get(ctx, id)
	if err != nil {
		return out, err
	}
	plan, result, _, err := parseRun(run)
	if err != nil {
		return out, err
	}
	var candidate *crawlagent.Candidate
	for i := range result.Candidates {
		if result.Candidates[i].Reference == ref {
			if candidate != nil {
				return out, agentstore.ErrCorrupt
			}
			candidate = &result.Candidates[i]
		}
	}
	if candidate == nil {
		return out, agentstore.ErrNotFound
	}
	var goal *crawlagent.StageGoal
	for i := range plan.Goal.Stages {
		g := &plan.Goal.Stages[i]
		if g.Stage == candidate.Stage && g.RuleID == ref.ID {
			goal = g
		}
	}
	if goal == nil {
		return out, agentstore.ErrCorrupt
	}
	detail, err := s.cfg.Rules.Inspect(ctx, ref)
	if err != nil {
		return out, err
	}
	out.Site, out.Candidate, out.Version, out.Shadow = plan.Goal.Site, candidateRef(*candidate), detail, candidate.Shadow
	for _, item := range []struct {
		id, set string
		target  **rulestore.Verification
	}{{candidate.TrainingReportID, goal.TrainingSet, &out.Training}, {candidate.HoldoutReportID, goal.HoldoutSet, &out.Holdout}} {
		if item.id == "" {
			continue
		}
		v, err := s.cfg.Rules.Verification(ctx, item.id)
		if err != nil {
			return out, err
		}
		if item.set == "" || v.SetID != item.set || v.Reference != ref || v.RuleDigest != detail.Version.Digest {
			return out, rulestore.ErrCorrupt
		}
		*item.target = &v
	}
	if goal.Existing != nil {
		old, err := s.cfg.Rules.Inspect(ctx, *goal.Existing)
		if err != nil {
			return out, err
		}
		a, err := crawlrule.Compile(old.Version.Rule)
		if err != nil {
			return out, rulestore.ErrCorrupt
		}
		b, err := crawlrule.Compile(detail.Version.Rule)
		if err != nil {
			return out, rulestore.ErrCorrupt
		}
		comparison, err := rulestore.CompareRules(a, b)
		if err != nil {
			return out, err
		}
		out.Comparison = &comparison
	}
	out.Publishable = run.Status == agentstore.AwaitingReview && result.Verified && candidate.HoldoutPassed && out.Holdout != nil && out.Holdout.Passed
	if goal.TrainingSet != "" && (out.Training == nil || !out.Training.Passed || !candidate.TrainingPassed) {
		out.Publishable = false
	}
	if out.Publishable {
		if detail.Verification == nil || detail.Verification.ID != out.Holdout.ID {
			out.Publishable = false
			out.BlockReason = "latest_verification_changed"
		} else {
			out.BlockReason = ""
		}
	}
	return out, nil
}

// Publication only prepares a complete manifest. It has no Change/Publish call.
func (s *Service) Publication(ctx context.Context, id string, ref rulestore.Reference) (rulestore.RulePlan, error) {
	review, err := s.Review(ctx, id, ref)
	if err != nil {
		return rulestore.RulePlan{}, err
	}
	if !review.Publishable {
		return rulestore.RulePlan{}, rulestore.ErrUnverified
	}
	current, err := s.cfg.Rules.Releases(ctx, review.Site, 0)
	if err != nil {
		return rulestore.RulePlan{}, err
	}
	if current.Current == nil {
		return rulestore.RulePlan{}, rulestore.ErrNoRelease
	}
	// Do not silently resurrect a removed/reassigned stage.
	binding, ok := current.Current.Bindings[review.Candidate.Stage]
	if !ok || binding.ID != ref.ID {
		return rulestore.RulePlan{}, ErrReviewChanged
	}
	bindings := make(map[string]rulestore.Reference, len(current.Current.Bindings))
	for stage, value := range current.Current.Bindings {
		bindings[stage] = value
	}
	bindings[review.Candidate.Stage] = ref
	return rulestore.RulePlan{Kind: "publish", Site: review.Site, Generation: current.Current.Generation, Bindings: bindings}, nil
}
