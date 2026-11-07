package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/pkg/agenteval"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
	"gorm.io/gorm"
)

type evaluationOutput struct {
	CorpusVersion string                `json:"corpus_version"`
	Manifest      []evaluationManifest  `json:"manifest"`
	Report        agenteval.Report      `json:"report"`
	Summary       []agenteval.Summary   `json:"summary"`
	Comparison    *agenteval.Comparison `json:"comparison,omitempty"`
}
type evaluationManifest struct{ Scenario, Split, Name, SHA256 string }

var errEvaluationQuality = errors.New("evaluation candidate/model quality failed; inspect report")

func validateEvaluationOptions(o options) error {
	if !o.Evaluate {
		if o.EvalHistory != "" || o.EvalCompare != "" || o.PriceVersion != "" || o.Currency != "" || o.InputPrice != 0 || o.OutputPrice != 0 || o.MaxCost != 0 {
			return errors.New("evaluation pricing/comparison flags require -evaluate")
		}
		return nil
	}
	if !o.Render || o.InspectRun != "" || o.InterruptRun != "" {
		return errors.New("fixed evaluation requires rendering and cannot inspect/interrupt a run")
	}
	if o.EvalHistory != "" && o.EvalHistory != "none" && o.EvalHistory != "published" {
		return errors.New("eval-history must be none or published")
	}
	if o.RealModel {
		if len(o.PriceVersion) == 0 || len(o.PriceVersion) > 64 || len(o.Currency) < 3 || len(o.Currency) > 16 || o.InputPrice <= 0 || o.OutputPrice <= 0 || o.MaxCost <= 0 || o.InputPrice > 1_000_000_000 || o.OutputPrice > 1_000_000_000 || o.MaxCost > 1_000_000_000 {
			return errors.New("real evaluation requires versioned prices, currency and a bounded per-run cost ceiling")
		}
		if _, err := evaluationEndpoint(o.BaseURL); err != nil {
			return err
		}
	} else if o.PriceVersion != "" || o.Currency != "" || o.InputPrice != 0 || o.OutputPrice != 0 || o.MaxCost != 0 {
		return errors.New("Replay evaluation does not estimate vendor cost")
	}
	return nil
}

func readEvaluation(path string) (*evaluationOutput, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read evaluation baseline")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("evaluation baseline exceeds limit")
	}
	var out evaluationOutput
	if json.Unmarshal(b, &out) != nil || out.Report.ValidateComplete() != nil {
		return nil, errors.New("invalid or incomplete evaluation baseline")
	}
	return &out, nil
}

func evaluationReplay(base string, s evaluationScenario) []model.ReplayStep {
	var actions []map[string]any
	mode := "http"
	if s.Transport == "browser" {
		mode = "render"
	}
	for _, f := range s.Training[:2] {
		a := map[string]any{"tool": "fetch_page", "url": base + f.Path, "mode": mode}
		if mode == "render" {
			a["wait"] = map[string]any{"selector": "section.card > h2", "state": "visible", "timeout_ms": 3000}
		}
		actions = append(actions, a, map[string]any{"tool": "read_evidence", "evidence_id": "latest", "limit": 16384})
	}
	// Replay alone uses this as its scripted answer. D4's explicitly approved
	// published-history arm may separately disclose the host-authored structure.
	raw, _ := json.Marshal(s.Oracle)
	var rule map[string]any
	_ = json.Unmarshal(raw, &rule)
	delete(rule, "id")
	delete(rule, "version")
	actions = append(actions, map[string]any{"tool": "submit_candidate", "stage": s.ID, "rule": rule}, map[string]any{"tool": "validate_candidate", "stage": s.ID}, map[string]any{"tool": "finish", "note": "Evaluate independently; leave candidate unpublished."})
	var steps []model.ReplayStep
	for _, a := range actions {
		raw, _ := json.Marshal(a)
		steps = append(steps, model.ReplayStep{Reply: model.Reply{Content: string(raw)}})
	}
	return steps
}

func runEvaluation(parent context.Context, w io.Writer, db *gorm.DB, o options) error {
	baseline, err := readEvaluation(o.EvalCompare)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	site := newEvaluationSite()
	defer site.Close()
	blobs, err := rulestore.NewFileBlobs(o.SampleDir)
	if err != nil {
		return err
	}
	defer blobs.Close()
	repo, err := rulestore.New(db, blobs)
	if err != nil {
		return err
	}
	audit, err := agentstore.New(db)
	if err != nil {
		return err
	}
	actor := rulestore.Actor{Operator: "evaluation-host", Source: "fixed synthetic evaluation"}
	policy := htmlfetch.SamplePolicy{QueryKeys: []string{"page"}, RedactBody: func(_ bool, b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }}
	out := evaluationOutput{CorpusVersion: evaluationCorpusVersion}
	for _, s := range evaluationCorpus() {
		for _, split := range []struct {
			name string
			fs   []evaluationFixture
		}{{"training", s.Training}, {"holdout", s.Holdout}} {
			for _, f := range split.fs {
				out.Manifest = append(out.Manifest, evaluationManifest{s.ID, split.name, f.Name, evaluationDigest(f)})
			}
		}
	}
	var finalErr error
cases:
	for _, s := range evaluationCorpus() {
		for repetition := 1; repetition <= 3; repetition++ {
			if ctx.Err() != nil {
				finalErr = errors.New("evaluation total time exhausted")
				break cases
			}
			run, id, runErr := runEvaluationCase(ctx, repo, blobs, audit, actor, policy, site.URL, o, s, repetition, baseline)
			if out.Report.Identity.Protocol == "" {
				out.Report.Identity = id
			}
			out.Report.Runs = append(out.Report.Runs, run)
			if runErr != nil {
				finalErr = runErr
			}
			if run.Failure == "incomparable_baseline" {
				break cases
			}
		}
	}
	if summary, err := out.Report.Summaries(); err == nil {
		out.Summary = summary
	} else if finalErr == nil {
		finalErr = err
	}
	if baseline != nil {
		compare := agenteval.Compare
		if o.EvalHistory == "published" && baseline.Report.Identity.HistoryMode == "none" {
			compare = agenteval.CompareHistory
		}
		comparison, err := compare(baseline.Report, out.Report)
		if err != nil {
			if finalErr == nil {
				finalErr = err
			}
		} else {
			out.Comparison = &comparison
		}
	}
	if err := printJSON(w, out); err != nil {
		return err
	}
	return finalErr
}

func runEvaluationCase(ctx context.Context, repo *rulestore.Repository, blobs *rulestore.FileBlobs, audit agentstore.Store, actor rulestore.Actor, policy htmlfetch.SamplePolicy, base string, o options, s evaluationScenario, repetition int, baseline *evaluationOutput) (entry agenteval.Run, identity agenteval.Identity, err error) {
	started := time.Now()
	prefix := "eval-" + uuid.NewString()
	entry = agenteval.Run{Scenario: s.ID, Repetition: repetition, RunID: prefix, Status: "not_started"}
	hidden := evaluationFixtures(base, s, s.Holdout)
	entry.Scores, _ = agenteval.Score(nil, hidden)
	defer func() {
		entry.ElapsedMS = time.Since(started).Milliseconds()
		if err != nil {
			if !errors.Is(err, errEvaluationQuality) {
				entry.InfrastructureFailure = true
			}
			if entry.Failure == "" {
				entry.Failure = "evaluation_failed"
			}
			entry.ReadyForReview = false
		}
	}()
	stage := crawlagent.StageGoal{Stage: s.ID, RuleID: prefix + "-rule", Transport: s.Transport, Fields: s.Fields, TrainingSet: prefix + "-train", HoldoutSet: prefix + "-holdout"}
	goal := crawlagent.Goal{Site: prefix, Stages: []crawlagent.StageGoal{stage}, Seeds: []string{base + s.Training[0].Path}, Context: evaluationGoalContext}
	provider, name, err := selectProvider(o, evaluationReplay(base, s))
	if err != nil {
		return entry, identity, errors.New("evaluation provider configuration failed")
	}
	origin := webaccess.Origin{URL: base, AllowPrivateNetwork: true, PathPrefixes: []string{"/" + s.ID + "/train", "/favicon.ico"}}
	budgets := crawlagent.Budgets{MaxModelCalls: 12, MaxDuration: 180 * time.Second}
	if o.RealModel {
		budgets.Prices = crawlagent.Prices{InputPerMillionMicros: o.InputPrice, OutputPerMillionMicros: o.OutputPrice, MaxMicros: o.MaxCost}
	}
	agentConfig := crawlagent.Config{Model: provider, ModelName: name, Audit: audit, Repository: repo, Blobs: blobs, Actor: actor, SamplePolicy: policy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{origin}, ResourceOrigins: []webaccess.Origin{origin}}, Renderer: webaccess.RendererConfig{Enabled: true, ExecutablePath: o.BrowserPath, GlobalConcurrency: 1}}, Budgets: budgets}
	agent, err := crawlagent.New(agentConfig)
	if err != nil {
		return entry, identity, errors.New("evaluation host configuration failed")
	}
	plan, err := agent.Plan(goal)
	if err != nil {
		return entry, identity, err
	}
	identity = agenteval.Identity{Protocol: agenteval.ProtocolVersion, CorpusSHA256: corpusDigest(), PromptSHA256: crawlagent.PromptDigest(), ToolProtocol: crawlagent.ToolProtocolVersion, Mode: "replay", Model: name, EndpointSHA256: evaluationDigest("local-replay"), OutputTokenField: "max_completion_tokens", PriceVersion: o.PriceVersion, Currency: o.Currency, Budgets: plan.Budgets, WebBudgets: plan.WebBudgets, Scenarios: []string{"static-html", "rendered-dom", "json-pagination"}, Repetitions: 3}
	if o.EvalHistory != "" {
		identity.HistoryMode = o.EvalHistory
		identity.HistoryFixtureSHA256 = evaluationHistoryDigest()
	}
	if o.LegacyMaxTokens {
		identity.OutputTokenField = "max_tokens"
	}
	if o.RealModel {
		identity.Mode = "real-compatible-model"
		identity.EndpointSHA256, _ = evaluationEndpoint(o.BaseURL)
	}
	if baseline != nil && !reflect.DeepEqual(baseline.Report.Identity, identity) && !(o.EvalHistory == "published" && agenteval.ComparableHistoryIdentity(baseline.Report.Identity, identity)) {
		entry.Failure = "incomparable_baseline"
		return entry, identity, errors.New("baseline identity differs; no model call made")
	}
	for _, set := range []struct {
		id       string
		fixtures []crawlrule.Fixture
	}{{stage.TrainingSet, evaluationFixtures(base, s, s.Training)}, {stage.HoldoutSet, hidden}} {
		var stored []rulestore.StoredFixture
		for _, f := range set.fixtures {
			sample, err := repo.AddSample(ctx, rulestore.SampleInput{URL: f.Response.URL.String(), StatusCode: f.Response.StatusCode, Body: f.Response.Body, Rendered: f.Rendered}, policy, actor)
			if err != nil {
				return entry, identity, errors.New("evaluation sample persistence failed")
			}
			stored = append(stored, rulestore.StoredFixture{Name: f.Name, SampleID: sample.ID, Expect: f.Expect})
		}
		if err := repo.CreateSampleSet(ctx, set.id, stored, actor); err != nil {
			return entry, identity, errors.New("evaluation set persistence failed")
		}
		saved, err := repo.SampleSet(ctx, set.id)
		if err != nil {
			return entry, identity, err
		}
		if set.id == stage.TrainingSet {
			entry.TrainingSet, entry.TrainingDigest = saved.ID, saved.Digest
		} else {
			entry.HoldoutSet, entry.HoldoutDigest = saved.ID, saved.Digest
		}
	}
	var initialRelease rulestore.Release
	if o.EvalHistory != "" {
		seed, release, seedErr := installEvaluationHistory(ctx, repo, prefix, s.ID, stage.TrainingSet, s.Oracle, actor)
		if seedErr != nil {
			return entry, identity, errors.New("synthetic history prerequisite failed")
		}
		initialRelease = release
		goal.Stages[0].Existing = &seed.Reference
		if o.EvalHistory == "published" {
			agentConfig.HistoryPolicy = approveEvaluationHistory(repo, prefix, s.ID, seed)
			agent, err = crawlagent.New(agentConfig)
			if err != nil {
				return entry, identity, errors.New("history configuration failed")
			}
		}
	}
	result, runErr := agent.RunWithID(ctx, prefix, goal)
	entry.Status = result.Status
	entry.Failure = result.Reason
	entry.InputBytes = result.Usage.InputBytes
	entry.WebUsage = webaccess.Usage{Requests: result.Usage.Requests, Pages: result.Usage.Pages, Bytes: result.Usage.Bytes}
	// Re-read committed audit, including failures; never rerun or publish here.
	readCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	view, readErr := inspect(readCtx, audit, result.RunID)
	if readErr != nil {
		return entry, identity, errors.New("evaluation audit unavailable")
	}
	entry.Audit = agenteval.MeasureAudit(view.Steps)
	if entry.Audit.ModelCalls != result.Usage.ModelCalls || view.Run.Status != result.Status {
		return entry, identity, errors.New("evaluation audit inconsistent")
	}
	if len(result.Candidates) == 1 {
		candidate := result.Candidates[0]
		entry.Candidate = candidate.Reference
		entry.TrainingReportID = candidate.TrainingReportID
		entry.HoldoutReportID = candidate.HoldoutReportID
		v, getErr := repo.Get(readCtx, candidate.Reference)
		if getErr != nil || v.EverPublished {
			return entry, identity, errors.New("evaluation candidate unavailable or published")
		}
		entry.CandidateDigest = v.Digest
		compiled, compileErr := crawlrule.Compile(v.Rule)
		if compileErr != nil {
			return entry, identity, errors.New("evaluation candidate invalid")
		}
		entry.Scores, err = agenteval.Score(compiled, hidden)
		if err != nil {
			return entry, identity, err
		}
	}
	current, currentErr := repo.Current(readCtx, prefix)
	if o.EvalHistory == "" {
		if !errors.Is(currentErr, rulestore.ErrNoRelease) {
			return entry, identity, errors.New("evaluation unexpectedly has a release")
		}
	} else if currentErr != nil || current.ID != initialRelease.ID || current.Generation != initialRelease.Generation {
		return entry, identity, errors.New("evaluation changed the host's synthetic release")
	}
	entry.InfrastructureFailure = entry.Scores.ExecutionFailures > 0 || entry.Audit.Incomplete || entry.Audit.InfrastructureFailures > 0
	if entry.InfrastructureFailure {
		entry.Failure = "evaluation_infrastructure_failed"
	}
	entry.ReadyForReview = runErr == nil && !entry.InfrastructureFailure && result.Verified && result.Status == agentstore.AwaitingReview
	if o.RealModel && entry.Audit.Tokens != nil && result.Usage.CostKnown {
		cost := result.Usage.EstimatedMicros
		entry.EstimatedMicros = &cost
	}
	if entry.ReadyForReview {
		entry.Failure = ""
	}
	if runErr != nil && !entry.InfrastructureFailure {
		switch result.Reason {
		case "model_empty_response", "model_invalid_response", "model_refusal", "model_truncated", "model_tool_call":
			return entry, identity, errEvaluationQuality
		}
	}
	if runErr != nil || entry.InfrastructureFailure {
		return entry, identity, errors.New("evaluation execution failed; inspect run IDs")
	}
	if !o.RealModel && !entry.ReadyForReview {
		return entry, identity, errEvaluationQuality
	}
	return entry, identity, nil
}
