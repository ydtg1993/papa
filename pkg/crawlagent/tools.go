package crawlagent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func (e *execution) stage(name string) (StageGoal, bool) {
	for _, s := range e.goal.Stages {
		if s.Stage == name {
			return s, true
		}
	}
	return StageGoal{}, false
}

func (e *execution) dispatch(ctx context.Context, a action) (any, *Result, error) {
	switch a.Tool {
	case "fetch_page", "record_api":
		if strings.HasPrefix(a.URL, "seed:") {
			index, err := strconv.Atoi(strings.TrimPrefix(a.URL, "seed:"))
			if err != nil || index < 0 || index >= len(e.goal.Seeds) {
				return nil, nil, ErrAction
			}
			a.URL = e.goal.Seeds[index]
		}
		hidden, hiddenErr := e.isHiddenURL(a.URL)
		if !validText(a.URL, 16<<10) || hidden || hiddenErr != nil {
			return nil, nil, ErrAction
		}
		if (a.Mode == "render" || a.Tool == "record_api") && !e.agent.config.Web.Renderer.Enabled {
			return nil, nil, ErrAction
		}
		if a.Tool == "record_api" && !e.agent.config.AllowRecordAPI {
			return nil, nil, ErrAction
		}
		if a.Mode != "" && a.Mode != "http" && a.Mode != "render" {
			return nil, nil, ErrAction
		}
		if err := e.updateUsage(ctx, func(u *Usage) error {
			if u.Pages >= 32 {
				return ErrBudget
			}
			u.Pages++
			return nil
		}); err != nil {
			return nil, nil, err
		}
		var response webaccess.Response
		var captured []webaccess.Response
		var err error
		if a.Mode == "render" || a.Tool == "record_api" {
			opts := webaccess.RenderOptions{ObserveDuration: 500 * time.Millisecond, RecordAPI: a.Tool == "record_api"}
			if a.Wait != nil {
				wait := a.Wait.options()
				opts.Wait, opts.ObserveDuration = &wait, 0
			}
			response, captured, err = e.source.Render(ctx, a.URL, opts)
		} else {
			response, err = e.source.Get(ctx, a.URL)
		}
		if err != nil {
			return nil, nil, err
		}
		items := []Evidence{}
		item, err := e.saveEvidence(ctx, response, "live_page")
		if err != nil {
			return nil, nil, err
		}
		items = append(items, item)
		for _, r := range captured {
			item, err = e.saveEvidence(ctx, r, "live_api")
			if err != nil {
				return nil, nil, err
			}
			items = append(items, item)
		}
		return map[string]any{"evidence": items}, nil, nil
	case "read_evidence":
		item, body, err := e.readEvidence(ctx, a.EvidenceID)
		if err != nil {
			return nil, nil, err
		}
		limit := a.Limit
		if limit == 0 {
			limit = 16 << 10
		}
		if limit < 1 || limit > 32<<10 || a.Offset < 0 || a.Offset > len(body) {
			return nil, nil, ErrAction
		}
		end := a.Offset + limit
		if end > len(body) {
			end = len(body)
		}
		return map[string]any{"evidence_id": item.ID, "offset": a.Offset, "content": string(body[a.Offset:end]), "truncated": end < len(body), "total_bytes": len(body)}, nil, nil
	case "inspect_selector":
		out, err := e.inspect(ctx, a)
		return out, nil, err
	case "submit_candidate":
		out, err := e.submit(ctx, a)
		return out, nil, err
	case "validate_candidate":
		out, err := e.validate(ctx, a.Stage)
		return out, nil, err
	case "finish":
		out, err := e.checkFinal(ctx)
		if err != nil {
			if errors.Is(err, ErrAction) {
				return nil, nil, err
			}
			out = e.result(agentstore.Failed, "independent_verification_unavailable", false)
			return nil, &out, err
		}
		return map[string]any{"status": out.Status, "verified": out.Verified}, &out, nil
	default:
		return nil, nil, ErrAction
	}
}

func (e *execution) submit(ctx context.Context, a action) (any, error) {
	stage, ok := e.stage(a.Stage)
	if !ok || len(a.Rule) == 0 || len(e.evidence) == 0 {
		return nil, ErrAction
	}
	if e.counts[stage.Stage] >= e.agent.config.Budgets.MaxCandidatesPerStage {
		return nil, ErrBudget
	}
	var rule crawlrule.Rule
	if decodeStrict(a.Rule, &rule, 64<<10) != nil {
		return nil, ErrAction
	}
	if (rule.ID != "" && rule.ID != stage.RuleID) || rule.Version != "" {
		return nil, ErrAction
	}
	rule.ID = stage.RuleID
	rule.Version = e.run.ID + "-" + fmt.Sprint(e.counts[stage.Stage]+1)
	// Fix evaluation resource ceilings before invoking the compiler.
	if rule.MaxRows == 0 {
		rule.MaxRows = 1000
	}
	if rule.MaxBodyBytes == 0 {
		rule.MaxBodyBytes = 2 << 20
	}
	if rule.MaxIssues == 0 {
		rule.MaxIssues = 100
	}
	if rule.MaxEvidence == 0 {
		rule.MaxEvidence = 10000
	}
	if rule.MaxRows > 1000 || rule.MaxBodyBytes > 8<<20 || rule.MaxIssues > 100 || rule.MaxEvidence > 10000 || (rule.Next != nil && rule.Next.AllowExternal) {
		return nil, ErrAction
	}
	if err := checkRuleComplexity(rule); err != nil {
		return nil, err
	}
	c, err := crawlrule.Compile(rule)
	if err != nil {
		return map[string]any{"error": "candidate_compile_failed", "detail": clip(err.Error(), 2048)}, ErrAction
	}
	rule = c.Rule()
	if err = checkRuleGoal(rule, stage); err != nil {
		return map[string]string{"error": "candidate_contract_mismatch"}, err
	}
	e.counts[stage.Stage]++
	v, err := e.agent.config.Repository.CreateDraft(ctx, rule, e.agent.config.Actor)
	if err != nil {
		return nil, errors.New("crawlagent: candidate persistence failed")
	}
	candidate := Candidate{Stage: stage.Stage, Reference: v.Reference}
	e.candidates[stage.Stage] = candidate
	if stage.Existing != nil {
		current, frozen := e.history[stage.Stage]
		var err error
		if !frozen {
			current, err = e.agent.config.Repository.Get(ctx, *stage.Existing)
		}
		if err != nil || !current.EverPublished {
			return nil, ErrEvidence
		}
		if err := checkRuleComplexity(current.Rule); err != nil {
			return nil, ErrEvidence
		}
		old, err := crawlrule.Compile(current.Rule)
		if err != nil {
			return nil, ErrEvidence
		}
		diff, err := rulestore.CompareRules(old, c)
		if err != nil {
			return nil, ErrEvidence
		}
		candidate.Comparison = &diff
		for _, item := range e.evidence {
			if len(candidate.Shadow) >= 3 {
				break
			}
			if item.Rendered && (old.Rule().Format != "html" || rule.Format != "html") {
				continue
			}
			_, body, readErr := e.readEvidence(ctx, item.ID)
			if readErr != nil {
				return nil, readErr
			}
			if err := checkEvaluationBudget(current.Rule, body); err != nil {
				return nil, err
			}
			if err := checkEvaluationBudget(rule, body); err != nil {
				return nil, err
			}
			u, parseErr := url.Parse(item.URL)
			if parseErr != nil {
				return nil, ErrEvidence
			}
			report, shadowErr := rulestore.Shadow(old, c, rulestore.ShadowInput{Response: &htmlfetch.Response{URL: u, StatusCode: item.StatusCode, Body: body}, Rendered: item.Rendered})
			if shadowErr != nil {
				return nil, ErrEvidence
			}
			candidate.Shadow = append(candidate.Shadow, ShadowSummary{EvidenceID: item.ID, Report: report})
		}
		e.candidates[stage.Stage] = candidate
	}
	// Saving an immutable draft has not run either verification. A bare
	// Candidate's false booleans look like failed tests to a real model and
	// can provoke repeated submissions without ever requesting validation.
	return map[string]any{"candidate": candidate, "validation_status": "not_run", "next_action": map[string]string{"tool": "validate_candidate", "stage": stage.Stage}}, nil
}

func checkRuleGoal(rule crawlrule.Rule, stage StageGoal) error {
	if stage.Transport == "browser" && rule.Format != "html" {
		return ErrAction
	}
	if len(rule.Fields) != len(stage.Fields) {
		return ErrAction
	}
	fields := map[string]crawlrule.Field{}
	for _, f := range rule.Fields {
		fields[f.Name] = f
	}
	for _, goal := range stage.Fields {
		f, ok := fields[goal.Name]
		if !ok || (goal.Kind != "any" && f.Validation.Kind != goal.Kind) || (goal.Required && (!f.Validation.Required || f.Validation.AllowNull)) {
			return ErrAction
		}
	}
	if stage.ItemKey != "" {
		if _, ok := fields[stage.ItemKey]; !ok {
			return ErrAction
		}
	}
	for _, link := range stage.Links {
		f, ok := fields[link.Field]
		if !ok {
			return ErrAction
		}
		switch f.Validation.Kind {
		case "any", "string", "http-url":
		default:
			return ErrAction
		}
		if n := len(f.Transforms); n > 0 && (f.Transforms[n-1].Op == "number" || f.Transforms[n-1].Op == "bool") {
			return ErrAction
		}
		for _, name := range link.Meta {
			if _, ok := fields[name]; !ok {
				return ErrAction
			}
		}
	}
	return nil
}

func compactReport(report crawlrule.FixtureReport) any {
	results := make([]map[string]any, 0, len(report.Results))
	for _, r := range report.Results {
		failures := r.Failures
		if len(failures) > 8 {
			failures = failures[:8]
		}
		safe := make([]string, len(failures))
		for i, f := range failures {
			safe[i] = clip(f, 512)
		}
		results = append(results, map[string]any{"name": r.Name, "kind": r.Kind, "error_kind": r.ErrorKind, "status_code": r.StatusCode, "passed": r.Passed, "rows": r.Rows, "invalid_rows": r.InvalidRows, "failures": safe})
	}
	return map[string]any{"passed": report.Passed, "positive": report.Positive, "negative": report.Negative, "failures": report.Failures, "results": results}
}

func (e *execution) validate(ctx context.Context, name string) (any, error) {
	stage, ok := e.stage(name)
	candidate, exists := e.candidates[name]
	if !ok || !exists {
		return nil, ErrAction
	}
	if stage.TrainingSet != "" {
		if err := e.checkFixtureEvaluation(ctx, candidate.Reference, stage.TrainingSet); err != nil {
			return nil, err
		}
		v, err := e.agent.config.Repository.Verify(ctx, candidate.Reference, stage.TrainingSet, e.agent.config.Actor)
		if err != nil && !errors.Is(err, crawlrule.ErrFixtureFailed) {
			return nil, ErrEvidence
		}
		candidate.TrainingReportID = v.ID
		candidate.TrainingPassed = v.Passed
		e.candidates[name] = candidate
		out := map[string]any{"candidate": candidate, "verification": compactReport(v.Report), "scope": "fixed_training_expectations", "validation_status": "training_failed"}
		if v.Passed {
			out["validation_status"] = "training_passed_holdout_not_run"
			ready := true
			for _, goal := range e.goal.Stages {
				c, exists := e.candidates[goal.Stage]
				if !exists || goal.TrainingSet != "" && !c.TrainingPassed {
					ready = false
					break
				}
			}
			if ready {
				out["next_action"] = map[string]string{"tool": "finish"}
			}
		}
		return out, nil
	}
	v, err := e.agent.config.Repository.Get(ctx, candidate.Reference)
	if err != nil {
		return nil, ErrEvidence
	}
	c, err := crawlrule.Compile(v.Rule)
	if err != nil {
		return nil, ErrEvidence
	}
	observations := []map[string]any{}
	for _, item := range e.evidence {
		if len(observations) >= 8 {
			break
		}
		_, body, err := e.readEvidence(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		if err := checkEvaluationBudget(v.Rule, body); err != nil {
			return nil, err
		}
		var out crawlrule.Result
		if item.Rendered {
			out, err = c.EvaluateRendered(item.URL, body)
		} else {
			u, parseErr := url.Parse(item.URL)
			if parseErr != nil {
				return nil, ErrEvidence
			}
			out, err = c.Evaluate(&htmlfetch.Response{URL: u, StatusCode: item.StatusCode, Body: body})
		}
		observations = append(observations, map[string]any{"evidence_id": item.ID, "success": err == nil, "rows": len(out.Records), "invalid_rows": out.Validation.InvalidRows, "truncated": out.Truncated})
	}
	return map[string]any{"scope": "structural_only", "observations": observations, "independent_validation": false}, nil
}

func (e *execution) checkFinal(ctx context.Context) (Result, error) {
	for _, s := range e.goal.Stages {
		c, ok := e.candidates[s.Stage]
		if !ok || (s.Existing != nil && c.Comparison == nil) {
			return Result{}, ErrAction
		}
	}
	missing := false
	passed := true
	for _, s := range e.goal.Stages {
		candidate := e.candidates[s.Stage]
		if s.HoldoutSet == "" {
			missing = true
			continue
		}
		if err := e.checkFixtureEvaluation(ctx, candidate.Reference, s.HoldoutSet); err != nil {
			if errors.Is(err, ErrBudget) {
				return Result{}, err
			}
			return Result{}, ErrEvidence
		}
		v, err := e.agent.config.Repository.Verify(ctx, candidate.Reference, s.HoldoutSet, e.agent.config.Actor)
		if err != nil && !errors.Is(err, crawlrule.ErrFixtureFailed) {
			return Result{}, ErrEvidence
		}
		candidate.HoldoutReportID = v.ID
		candidate.HoldoutPassed = v.Passed
		e.candidates[s.Stage] = candidate
		passed = passed && v.Passed
	}
	if missing {
		return e.result(agentstore.NeedsFixtures, "independent_fixtures_required", false), nil
	}
	if !passed {
		return e.result(agentstore.AwaitingReview, "independent_validation_failed", false), nil
	}
	return e.result(agentstore.AwaitingReview, "independent_validation_passed", true), nil
}
