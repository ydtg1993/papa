package crawlagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

func TestHistoryDisclosureChangesReviewedPlan(t *testing.T) {
	cfg, goal, _, _, _ := runnerHarness(t, replayActions(`{"tool":"finish"}`))
	goal.Stages[0].Existing = &rulestore.Reference{ID: "old", Version: "v1"}
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.Plan(goal)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HistoryPolicy = approveExampleHistory
	a, err = crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err = a.ForActor(runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	after, err := a.Plan(goal)
	if err != nil {
		t.Fatal(err)
	}
	if before.HistoryEnabled || !after.HistoryEnabled || runnerJSON(before) == runnerJSON(after) {
		t.Fatal("history disclosure does not change reviewed plan")
	}
}

func TestRunnerHistoryUsesOriginalInputAndCostBudgets(t *testing.T) {
	for _, limit := range []string{"single_input", "total_input", "cost"} {
		t.Run(limit, func(t *testing.T) {
			calls, input := 0, 0
			provider := providerFunc(func(_ context.Context, messages []model.Message, _ int) (model.Reply, error) {
				calls++
				input = len(runnerJSON(messages))
				return model.Reply{}, model.ErrTransport
			})
			cfg, goal, repo, _, audit := runnerHarness(t, provider)
			v := historyVersion(t, repo)
			goal.Stages[0].Existing = &v.Reference
			_, _ = runAgent(t, cfg, goal)
			if calls != 1 || input == 0 {
				t.Fatal("control did not reach model")
			}
			switch limit {
			case "single_input":
				cfg.Budgets.MaxInputBytes = input
			case "total_input":
				cfg.Budgets.MaxTotalInputBytes = input
			case "cost":
				cfg.Budgets.Prices = crawlagent.Prices{InputPerMillionMicros: 1_000_000, OutputPerMillionMicros: 1_000_000, MaxMicros: int64(input + 4096 + 4096)}
			}
			calls = 0
			_, _ = runAgent(t, cfg, goal)
			if calls != 1 {
				t.Fatal("same control no longer fits original budget")
			}
			calls = 0
			cfg.HistoryPolicy = approveExampleHistory
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || result.Status != agentstore.Exhausted || calls != 0 || result.Usage.ModelCalls != 0 {
				t.Fatalf("history escaped original %s budget: %+v %v", limit, result, err)
			}
			steps, err := audit.Steps(context.Background(), result.RunID, 0)
			if err != nil || len(steps) != 1 || steps[0].Kind != "history" || steps[0].Status != agentstore.StepSucceeded {
				t.Fatal("prepared history incorrectly counted as a model call")
			}
		})
	}
}

func TestRunnerConcurrentHistorySnapshots(t *testing.T) {
	var calls atomic.Int32
	provider := providerFunc(func(_ context.Context, messages []model.Message, _ int) (model.Reply, error) {
		var initial struct {
			Goal    crawlagent.Goal
			History []struct{ Rule crawlrule.Rule }
		}
		if json.Unmarshal([]byte(messages[1].Content), &initial) != nil || len(initial.History) != 1 || initial.History[0].Rule.Fields[0].Selector != "."+initial.Goal.Context {
			t.Error("history crossed concurrent run boundary")
		}
		calls.Add(1)
		return model.Reply{}, model.ErrTransport
	})
	cfg, goal, repo, _, audit := runnerHarness(t, provider)
	cfg.SourceFactory = func(webaccess.Config) (crawlagent.Source, error) { return &runnerSource{}, nil }
	cfg.HistoryPolicy = approveExampleHistory
	base := historyVersion(t, repo)
	var goals []crawlagent.Goal
	for i := 0; i < 8; i++ {
		v := base
		v.Version = fmt.Sprintf("v%d", i)
		c, _ := crawlrule.Compile(base.Rule)
		v.Rule = c.Rule()
		v.Rule.Version = v.Version
		v.Rule.Fields[0].Selector = fmt.Sprintf(".variant%d", i)
		b, _ := json.Marshal(v.Rule)
		sum := sha256.Sum256(b)
		v.Digest = hex.EncodeToString(sum[:])
		repo.versions[v.Reference] = v
		g := goal
		g.Stages = append([]crawlagent.StageGoal(nil), goal.Stages...)
		g.Stages[0].Existing = &v.Reference
		g.Context = fmt.Sprintf("variant%d", i)
		goals = append(goals, g)
	}
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, g := range goals {
		wg.Add(1)
		go func(g crawlagent.Goal) {
			defer wg.Done()
			result, err := a.Run(context.Background(), g)
			if !errors.Is(err, model.ErrTransport) {
				t.Errorf("run: %v", err)
				return
			}
			steps, err := audit.Steps(context.Background(), result.RunID, 0)
			var record crawlagent.HistoryRecord
			if err != nil || len(steps) != 2 || json.Unmarshal([]byte(steps[0].OutputJSON), &record) != nil || record.Reference != *g.Stages[0].Existing {
				t.Error("audit history crossed runs")
			}
		}(g)
	}
	wg.Wait()
	if calls.Load() != 8 {
		t.Fatal("missing concurrent run")
	}
}

func TestRunnerHistoryRejectsEquivalentHiddenURL(t *testing.T) {
	for _, literal := range []string{"https://SITE.example:443/HIDDEN_URL?b=2&a=1", "https://site.example/HIDDEN_URL?b=2&a=1"} {
		t.Run(literal, func(t *testing.T) {
			replay := replayActions(`{"tool":"finish"}`)
			cfg, goal, repo, _, _ := runnerHarness(t, replay)
			cfg.SamplePolicy.QueryKeys = []string{"a", "b"}
			sample, err := repo.AddSample(context.Background(), rulestore.SampleInput{URL: "https://site.example/HIDDEN_URL?a=1&b=2", StatusCode: 200, Body: []byte("<article><b>HIDDEN</b></article>")}, cfg.SamplePolicy, runnerActor)
			if err != nil {
				t.Fatal(err)
			}
			repo.sets["hold"] = rulestore.SampleSet{ID: "hold", Fixtures: []rulestore.StoredFixture{{Name: "hidden", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "HIDDEN"}}}}}}
			goal.Stages[0].HoldoutSet = "hold"
			v := historyVersion(t, repo)
			v.Rule.Fields[0].Selector = `a[href="` + literal + `"]`
			c, err := crawlrule.Compile(v.Rule)
			if err != nil {
				t.Fatal(err)
			}
			v.Rule = c.Rule()
			b, _ := json.Marshal(v.Rule)
			sum := sha256.Sum256(b)
			v.Digest = hex.EncodeToString(sum[:])
			repo.versions[v.Reference] = v
			goal.Stages[0].Existing = &v.Reference
			cfg.HistoryPolicy = approveExampleHistory
			_, err = runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrEvidence) || len(replay.Calls()) != 0 {
				t.Fatal("equivalent hidden URL disclosed to model")
			}
		})
	}
}

func historyVersion(t *testing.T, repo *runnerRepository) rulestore.Version {
	t.Helper()
	v, err := repo.CreateDraft(context.Background(), crawlrule.Rule{SchemaVersion: 1, ID: "PRIVATE_HISTORY_ID", Version: "PRIVATE_VERSION", Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "i", Validation: crawlrule.Validation{Kind: "string", Required: true, Pattern: "PRIVATE_PATTERN"}}}}, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	v.State, v.EverPublished = "published", true
	v.Actor.Source = "PRIVATE_ACTOR"
	raw, _ := json.Marshal(v.Rule)
	sum := sha256.Sum256(raw)
	v.Digest = hex.EncodeToString(sum[:])
	repo.versions[v.Reference] = v
	return v
}

func approveExampleHistory(_ context.Context, r crawlagent.HistoryRequest) error {
	if r.Site != "example" || r.Stage != "catalog" || r.Version.ID != "PRIVATE_HISTORY_ID" {
		return errors.New("not approved")
	}
	return nil
}

func TestRunnerHistoryOptInProjectionAuditAndFreeze(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default_comparison_only", true: "approved_history"}[enabled], func(t *testing.T) {
			replay := replayActions(candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`, `{"tool":"finish"}`)
			cfg, goal, repo, source, audit := runnerHarness(t, replay)
			addRunnerFixture(t, repo, "train", "https://site.example/train", "<article><b>TRAIN</b><i>old</i></article>", "TRAIN")
			hidden := addRunnerFixture(t, repo, "HIDDEN_SET", "https://site.example/HIDDEN_URL", "<article><b>HIDDEN_ANSWER</b></article>", "HIDDEN_ANSWER")
			goal.Stages[0].TrainingSet, goal.Stages[0].HoldoutSet = "train", "HIDDEN_SET"
			old := historyVersion(t, repo)
			goal.Stages[0].Existing = &old.Reference
			approvals := 0
			if enabled {
				cfg.HistoryPolicy = func(ctx context.Context, r crawlagent.HistoryRequest) error {
					approvals++
					if err := approveExampleHistory(ctx, r); err != nil {
						return err
					}
					if strings.Contains(string(r.ViewJSON), "PRIVATE_") {
						t.Fatal("private metadata in projection")
					}
					// An approving callback cannot change the frozen model/shadow input.
					r.Version.Rule.Fields[0].Selector = "MUTATED_BY_CALLBACK"
					for i := range r.ViewJSON {
						r.ViewJSON[i] = ' '
					}
					return nil
				}
				cfg.Model = providerFunc(func(ctx context.Context, m []model.Message, n int) (model.Reply, error) {
					// Simulates a concurrently changing backend after initial approval.
					repo.mu.Lock()
					v := repo.versions[old.Reference]
					v.Rule.Fields[0].Selector = "MUTATED_BACKEND"
					repo.versions[old.Reference] = v
					repo.mu.Unlock()
					return replay.Complete(ctx, m, n)
				})
			}
			result, err := runAgent(t, cfg, goal)
			if err != nil || !result.Verified || source.calls.Load() != 0 {
				t.Fatalf("history result: %+v %v", result, err)
			}
			var initial struct {
				History []struct {
					Stage string
					State string
					Rule  crawlrule.Rule
				} `json:"history"`
			}
			if json.Unmarshal([]byte(replay.Calls()[0].Messages[1].Content), &initial) != nil {
				t.Fatal("bad prompt")
			}
			if enabled {
				if approvals != 1 || len(initial.History) != 1 || initial.History[0].Rule.Fields[0].Selector != "i" {
					t.Fatalf("history not frozen: %+v", initial)
				}
			} else if approvals != 0 || len(initial.History) != 0 {
				t.Fatal("implicit disclosure")
			}
			transcript := runnerJSON(replay.Calls())
			// Candidate comparison already exposes an old digest, but never identity,
			// validation pattern, author, hidden samples or hidden expected values.
			for _, secret := range []string{"PRIVATE_HISTORY_ID", "PRIVATE_VERSION", "PRIVATE_PATTERN", "PRIVATE_ACTOR", "HIDDEN_SET", "HIDDEN_ANSWER", hidden.URL, hidden.ID, "MUTATED_BACKEND", "MUTATED_BY_CALLBACK"} {
				if strings.Contains(transcript, secret) {
					t.Fatalf("history leaked %s", secret)
				}
			}
			steps, err := audit.Steps(context.Background(), result.RunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := 6
			if enabled {
				want++
			}
			if len(steps) != want {
				t.Fatalf("steps=%d want %d", len(steps), want)
			}
			if enabled {
				var record crawlagent.HistoryRecord
				if json.Unmarshal([]byte(steps[0].OutputJSON), &record) != nil || steps[0].Kind != "history" || steps[0].Status != agentstore.StepSucceeded || record.Reference != old.Reference || record.RuleSHA256 != old.Digest || record.ViewBytes <= 0 {
					t.Fatalf("missing durable history: %+v", record)
				}
			}
			before := len(replay.Calls())
			_, _ = audit.Get(context.Background(), result.RunID)
			_, _ = audit.Steps(context.Background(), result.RunID, 0)
			if len(replay.Calls()) != before {
				t.Fatal("inspection replayed model")
			}
		})
	}
}

func TestRunnerHistoryRejectedBeforeModelAndSource(t *testing.T) {
	for _, kind := range []string{"denied", "panic", "unpublished", "wrong_site", "wrong_stage", "corrupt_digest", "hidden_url", "view_budget", "input_budget", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			replay := replayActions(`{"tool":"finish"}`)
			cfg, goal, repo, source, audit := runnerHarness(t, replay)
			addRunnerFixture(t, repo, "hold", "https://site.example/HIDDEN_URL", "<article><b>HIDDEN_ANSWER</b></article>", "HIDDEN_ANSWER")
			goal.Stages[0].HoldoutSet = "hold"
			v := historyVersion(t, repo)
			goal.Stages[0].Existing = &v.Reference
			cfg.HistoryPolicy = approveExampleHistory
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "denied":
				cfg.HistoryPolicy = func(context.Context, crawlagent.HistoryRequest) error { return errors.New("SECRET reason") }
			case "panic":
				cfg.HistoryPolicy = func(context.Context, crawlagent.HistoryRequest) error { panic("SECRET callback panic") }
			case "unpublished":
				v.EverPublished = false
				v.State = "draft"
			case "wrong_site":
				goal.Site = "another-site"
			case "wrong_stage":
				goal.Stages[0].Stage = "another-stage"
			case "corrupt_digest":
				v.Digest = "bad"
			case "hidden_url":
				v.Rule.Fields[0].Selector = `a[href="https://site.example/HIDDEN_URL"]`
			case "view_budget":
				v.Rule.Fields = nil
				for i := 0; i < 32; i++ {
					v.Rule.Fields = append(v.Rule.Fields, crawlrule.Field{Name: string(rune('a' + i)), Selector: "." + strings.Repeat("a", 480), Validation: crawlrule.Validation{Kind: "string"}})
				}
			case "input_budget":
				cfg.Budgets.MaxInputBytes = 1
			case "canceled":
				cfg.HistoryPolicy = func(context.Context, crawlagent.HistoryRequest) error { cancel(); return nil }
			}
			if kind == "hidden_url" || kind == "view_budget" {
				c, err := crawlrule.Compile(v.Rule)
				if err != nil {
					t.Fatal(err)
				}
				v.Rule = c.Rule()
				b, _ := json.Marshal(v.Rule)
				sum := sha256.Sum256(b)
				v.Digest = hex.EncodeToString(sum[:])
			}
			repo.versions[v.Reference] = v
			a, err := crawlagent.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := a.Run(ctx, goal)
			if err == nil || result.Verified || len(replay.Calls()) != 0 || source.calls.Load() != 0 {
				t.Fatalf("bad history escaped: %+v %v", result, err)
			}
			run, readErr := audit.Get(context.Background(), result.RunID)
			if readErr != nil || strings.Contains(run.ResultJSON, "SECRET") {
				t.Fatal("lost or unsafe audit")
			}
		})
	}
}

func TestRunnerHistoryCountAndTotalBudget(t *testing.T) {
	for _, kind := range []string{"count", "total"} {
		t.Run(kind, func(t *testing.T) {
			replay := replayActions(`{"tool":"finish"}`)
			cfg, goal, repo, source, _ := runnerHarness(t, replay)
			v := historyVersion(t, repo)
			count := crawlagent.MaxHistoryRules + 1
			if kind == "total" {
				count = 3
				v.Rule.Fields = nil
				for i := 0; i < 10; i++ {
					v.Rule.Fields = append(v.Rule.Fields, crawlrule.Field{Name: string(rune('a' + i)), Selector: "." + strings.Repeat("a", 500), Validation: crawlrule.Validation{Kind: "string"}})
				}
				c, err := crawlrule.Compile(v.Rule)
				if err != nil {
					t.Fatal(err)
				}
				v.Rule = c.Rule()
				raw, _ := json.Marshal(v.Rule)
				sum := sha256.Sum256(raw)
				v.Digest = hex.EncodeToString(sum[:])
				repo.versions[v.Reference] = v
			}
			stage := goal.Stages[0]
			goal.Stages = nil
			for i := 0; i < count; i++ {
				s := stage
				s.Stage = string(rune('a' + i))
				s.RuleID = "rule-" + s.Stage
				s.Existing = &v.Reference
				goal.Stages = append(goal.Stages, s)
			}
			cfg.HistoryPolicy = func(context.Context, crawlagent.HistoryRequest) error { return nil }
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || result.Verified || len(replay.Calls()) != 0 || source.calls.Load() != 0 {
				t.Fatalf("history %s budget escaped: %+v %v", kind, result, err)
			}
		})
	}
}

func TestRunnerHistoryAuditFailurePreventsModel(t *testing.T) {
	for _, when := range []string{"begin", "end"} {
		t.Run(when, func(t *testing.T) {
			replay := replayActions(`{"tool":"finish"}`)
			cfg, goal, repo, source, audit := runnerHarness(t, replay)
			v := historyVersion(t, repo)
			goal.Stages[0].Existing = &v.Reference
			approval := 0
			cfg.HistoryPolicy = func(context.Context, crawlagent.HistoryRequest) error { approval++; return nil }
			fail := &failingAudit{Store: audit}
			if when == "begin" {
				fail.beginAt = 1
			} else {
				fail.endAt = 1
			}
			cfg.Audit = fail
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrAudit) || len(replay.Calls()) != 0 || source.calls.Load() != 0 || result.Verified {
				t.Fatalf("unaudited history used: %+v %v", result, err)
			}
			if when == "begin" && approval != 0 {
				t.Fatal("approval ran before reservation")
			}
		})
	}
}

func TestRunnerMySQLHistoryAuditRecoveryWithoutReplay(t *testing.T) {
	db, repo, blobs, audit, prefix, _ := runnerMySQL(t)
	ctx := context.Background()
	createRunnerMySQLFixture(t, repo, prefix+"-train", "https://site.example/train", "TRAIN")
	v, err := repo.CreateDraft(ctx, crawlrule.Rule{SchemaVersion: 1, ID: prefix + "-old", Version: "v1", Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "b", Validation: crawlrule.Validation{Kind: "string", Required: true}}}}, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := repo.Verify(ctx, v.Reference, prefix+"-train", runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := repo.PublishReviewed(ctx, prefix+"-seed", prefix, 0, map[string]rulestore.Reference{"catalog": v.Reference}, map[string]string{"catalog": proof.ID}, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	replay := replayActions(`{"tool":"finish"}`)
	failed := &failingAudit{Store: audit, endAt: 1}
	cfg := crawlagent.Config{Model: replay, ModelName: "history-recovery", Audit: failed, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://site.example"}}}}, HistoryPolicy: func(_ context.Context, request crawlagent.HistoryRequest) error {
		if request.Site != prefix || request.Stage != "catalog" || request.Version.Reference != v.Reference {
			return crawlagent.ErrEvidence
		}
		return nil
	}}
	goal := crawlagent.Goal{Site: prefix, Seeds: []string{"https://site.example/list"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-new", Existing: &v.Reference, TrainingSet: prefix + "-train", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RunWithID(ctx, prefix+"-run", goal)
	if !errors.Is(err, crawlagent.ErrAudit) || result.Status != agentstore.Interrupted || len(replay.Calls()) != 0 {
		t.Fatalf("uncertain history: %+v %v", result, err)
	}
	reopened, err := agentstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reopened.Get(ctx, result.RunID)
	if err != nil || run.Status != agentstore.Running {
		t.Fatalf("pending run lost: %+v %v", run, err)
	}
	var plan crawlagent.Plan
	if json.Unmarshal([]byte(run.SnapshotJSON), &plan) != nil || !plan.HistoryEnabled || *plan.Goal.Stages[0].Existing != v.Reference {
		t.Fatal("reopened history intent/reference changed")
	}
	steps, err := reopened.Steps(ctx, run.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Kind != "history" || steps[0].Status != agentstore.StepPending {
		t.Fatal("uncertain history not preserved")
	}
	// Run has returned: the host can now explicitly attest that this worker stopped.
	if _, err := reopened.Interrupt(ctx, run.ID, run.Revision); err != nil {
		t.Fatal(err)
	}
	steps, err = reopened.Steps(ctx, run.ID, 0)
	if err != nil || steps[0].Status != agentstore.StepUnknown {
		t.Fatal("uncertain history converted into success")
	}
	cfg.Audit = reopened
	a, err = crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunWithID(ctx, run.ID, goal); !errors.Is(err, crawlagent.ErrAudit) || len(replay.Calls()) != 0 {
		t.Fatal("interrupted identity replayed model")
	}
	current, err := repo.Current(ctx, prefix)
	if err != nil || current.ID != seed.ID || current.Generation != seed.Generation {
		t.Fatal("audit recovery changed publication")
	}
}
