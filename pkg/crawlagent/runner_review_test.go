package crawlagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func reviewRecordPermission(t *testing.T, cfg *crawlagent.Config, allow bool) {
	t.Helper()
	field := reflect.ValueOf(cfg).Elem().FieldByName("AllowRecordAPI")
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatal("Config must expose independent AllowRecordAPI bool, default false")
	}
	field.SetBool(allow)
}

func TestRunnerReviewRenderAndRecordNeedSeparateHostPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, action   string
		render, record bool
		wantCalls      int
	}{
		{"render_default_denied", `{"tool":"fetch_page","url":"https://site.example/list","mode":"render"}`, false, false, 0},
		{"record_default_denied", `{"tool":"record_api","url":"https://site.example/list"}`, true, false, 0},
		{"record_cannot_enable_render", `{"tool":"record_api","url":"https://site.example/list"}`, false, true, 0},
		{"explicit_record_allowed", `{"tool":"record_api","url":"https://site.example/list"}`, true, true, 1},
		{"explicit_render_allowed", `{"tool":"fetch_page","url":"https://site.example/list","mode":"render"}`, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replay := replayActions(tc.action)
			cfg, goal, _, source, _ := runnerHarness(t, replay)
			cfg.Web.Renderer.Enabled = tc.render
			if tc.record {
				reviewRecordPermission(t, &cfg, true)
			}
			cfg.Budgets.MaxModelCalls = 1
			a, err := crawlagent.New(cfg)
			if err != nil {
				if tc.record && !tc.render && errors.Is(err, crawlagent.ErrInvalid) {
					return
				}
				t.Fatal(err)
			}
			result, err := a.Run(context.Background(), goal)
			if !errors.Is(err, crawlagent.ErrBudget) || result.Status != agentstore.Exhausted || source.calls.Load() != int64(tc.wantCalls) {
				t.Fatalf("permission boundary: %+v error=%v source calls=%d", result, err, source.calls.Load())
			}
		})
	}
}

func TestRunnerReviewHiddenURLAliasesCannotFetchDynamicBody(t *testing.T) {
	for name, alias := range map[string]string{
		"host_case":       "https://SITE.EXAMPLE/hidden",
		"default_port":    "https://site.example:443/hidden",
		"raw_path":        "https://site.example/%68idden",
		"discarded_nonce": "https://site.example/hidden?nonce=dynamic-2",
		"combined":        "https://SITE.EXAMPLE:443/%68idden?nonce=dynamic-2#fragment",
	} {
		t.Run(name, func(t *testing.T) {
			replay := replayActions(runnerJSON(map[string]any{"tool": "fetch_page", "url": alias}))
			cfg, goal, repo, source, _ := runnerHarness(t, replay)
			cfg.Web.Policy.Origins[0].PathPrefixes = nil
			addRunnerFixture(t, repo, "hidden", "https://site.example/hidden", "<article><b>FIXED_HIDDEN_BODY</b></article>", "FIXED_HIDDEN_BODY")
			goal.Stages[0].HoldoutSet = "hidden"
			cfg.Budgets.MaxModelCalls = 1
			source.allowedURL = alias
			source.body = "<article><b>CHANGED_NONCE_HIDDEN_BODY</b></article>"
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || source.calls.Load() != 0 || len(result.Evidence) != 0 {
				t.Fatalf("hidden URL alias fetched: result=%+v error=%v calls=%d", result, err, source.calls.Load())
			}
		})
	}
}

func TestRunnerReviewHiddenSeedAliasesFailBeforeModel(t *testing.T) {
	replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`)
	cfg, goal, repo, source, _ := runnerHarness(t, replay)
	cfg.Web.Policy.Origins[0].PathPrefixes = nil
	addRunnerFixture(t, repo, "hidden", "https://site.example/hidden", "<article><b>HIDDEN</b></article>", "HIDDEN")
	goal.Stages[0].HoldoutSet = "hidden"
	goal.Seeds[0] = "https://SITE.EXAMPLE:443/%68idden?nonce=another"
	source.allowedURL = goal.Seeds[0]
	source.body = "<article><b>dynamic</b></article>"
	result, err := runAgent(t, cfg, goal)
	if err == nil || len(replay.Calls()) != 0 || source.calls.Load() != 0 || result.Verified {
		t.Fatalf("hidden seed alias accepted: %+v %v calls=%d", result, err, len(replay.Calls()))
	}
}

func TestRunnerReviewHiddenURLIdentityUsesCurrentQueryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, id  string
		wantCalls int
	}{{"same_approved_query", "1", 0}, {"distinct_approved_query", "2", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			alias := "https://SITE.EXAMPLE:443/%68idden?id=" + tc.id + "&nonce=dynamic"
			replay := replayActions(runnerJSON(map[string]string{"tool": "fetch_page", "url": alias}))
			cfg, goal, repo, source, _ := runnerHarness(t, replay)
			cfg.Web.Policy.Origins[0].PathPrefixes = nil
			cfg.SamplePolicy.QueryKeys = []string{"id"}
			sample, err := repo.AddSample(context.Background(), rulestore.SampleInput{URL: "https://site.example/hidden?id=1", StatusCode: 200, Body: []byte("<article><b>FIXED_HIDDEN</b></article>")}, cfg.SamplePolicy, runnerActor)
			if err != nil {
				t.Fatal(err)
			}
			repo.sets["hidden"] = rulestore.SampleSet{ID: "hidden", Fixtures: []rulestore.StoredFixture{{Name: "hidden", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "FIXED_HIDDEN"}}}}}}
			goal.Stages[0].HoldoutSet = "hidden"
			cfg.Budgets.MaxModelCalls = 1
			source.allowedURL = alias
			source.body = "<article><b>DIFFERENT_BODY</b></article>"
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || source.calls.Load() != int64(tc.wantCalls) || len(result.Evidence) != tc.wantCalls {
				t.Fatalf("query identity: %+v %v calls=%d", result, err, source.calls.Load())
			}
		})
	}
}

func TestRunnerReviewProviderSuccessAfterCancellationDoesNotBeginTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := providerFunc(func(context.Context, []model.Message, int) (model.Reply, error) {
		cancel()
		return model.Reply{Content: `{"tool":"fetch_page","url":"https://site.example/list"}`, Usage: model.Usage{Known: true, InputTokens: 10, OutputTokens: 5}}, nil
	})
	cfg, goal, _, source, audit := runnerHarness(t, p)
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Run(ctx, goal)
	if !errors.Is(err, context.Canceled) || result.Status != agentstore.Canceled || source.calls.Load() != 0 || source.closed.Load() != 1 {
		t.Fatalf("canceled success reply: %+v %v calls=%d", result, err, source.calls.Load())
	}
	steps, err := audit.Steps(context.Background(), result.RunID, 0)
	if err != nil || len(steps) != 1 || steps[0].Kind != "model" || steps[0].Status != agentstore.StepFailed {
		t.Fatalf("tool began after cancellation: %+v %v", steps, err)
	}
	run, err := audit.Get(context.Background(), result.RunID)
	if err != nil || run.Status != agentstore.Canceled {
		t.Fatalf("cancellation audit: %+v %v", run, err)
	}
}

func TestRunnerReviewSnapshotPreservesActorWebBudgetsAndCapabilities(t *testing.T) {
	replay := replayActions(`{"tool":"unknown"}`)
	cfg, goal, _, _, audit := runnerHarness(t, replay)
	cfg.Actor = rulestore.Actor{Operator: "review-operator", Source: "review-source"}
	cfg.Web.Budgets = webaccess.Budgets{MaxPages: 3, MaxRequests: 9, MaxResponseBytes: 2048, MaxTotalBytes: 8192, MaxDuration: 12 * time.Second, MaxRedirects: 2}
	cfg.Web.Renderer = webaccess.RendererConfig{Enabled: true, GlobalConcurrency: 2}
	reviewRecordPermission(t, &cfg, true)
	cfg.Budgets.MaxModelCalls = 1
	result, err := runAgent(t, cfg, goal)
	if !errors.Is(err, crawlagent.ErrBudget) {
		t.Fatalf("run: %+v %v", result, err)
	}
	run, err := audit.Get(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Actor          rulestore.Actor
		WebBudgets     webaccess.Budgets
		Renderer       webaccess.RendererConfig
		AllowRecordAPI bool
	}
	if err := json.Unmarshal([]byte(run.SnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Actor != cfg.Actor || snapshot.WebBudgets != cfg.Web.Budgets || !snapshot.Renderer.Enabled || snapshot.Renderer.GlobalConcurrency != 2 || !snapshot.AllowRecordAPI {
		t.Fatalf("incomplete audit authority snapshot: %+v", snapshot)
	}
}

type reviewFinishFailure struct{ agentstore.Store }

func (s reviewFinishFailure) Finish(context.Context, string, uint64, string, string, string) (agentstore.Run, error) {
	return agentstore.Run{}, errors.New("injected final audit failure")
}

func reviewJSONContainsString(data, want string) bool {
	var v any
	if json.Unmarshal([]byte(data), &v) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(value any) bool {
		switch x := value.(type) {
		case string:
			return x == want
		case []any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		case map[string]any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(v)
}

func TestRunnerReviewCompletedStepsRetainArtifactReferencesBeforeFinish(t *testing.T) {
	for _, kind := range []string{"evidence", "candidate", "training_report", "holdout_report"} {
		t.Run(kind, func(t *testing.T) {
			actions := []string{}
			switch kind {
			case "evidence":
				actions = []string{`{"tool":"fetch_page","url":"https://site.example/list"}`}
			case "candidate":
				actions = []string{candidateAction("b")}
			case "training_report":
				actions = []string{candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`}
			case "holdout_report":
				actions = []string{candidateAction("b"), `{"tool":"finish"}`}
			}
			replay := replayActions(actions...)
			cfg, goal, repo, source, audit := runnerHarness(t, replay)
			source.body = "<article><b>REVIEW_FETCH_BODY</b></article>"
			if kind != "evidence" {
				addRunnerFixture(t, repo, "train", "https://site.example/train", "<article><b>REVIEW_TRAIN_BODY</b></article>", "REVIEW_TRAIN_BODY")
				goal.Stages[0].TrainingSet = "train"
			}
			if kind == "holdout_report" {
				addRunnerFixture(t, repo, "hold", "https://site.example/hold", "<article><b>REVIEW_HOLD_BODY</b></article>", "REVIEW_HOLD_BODY")
				goal.Stages[0].HoldoutSet = "hold"
				cfg.Audit = reviewFinishFailure{Store: audit}
			} else {
				cfg.Audit = &failingAudit{Store: audit, beginAt: 2*len(actions) + 1}
			}
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrAudit) || result.Status != agentstore.Interrupted {
				t.Fatalf("expected pre-finish stop: %+v %v", result, err)
			}
			run, err := audit.Get(context.Background(), result.RunID)
			if err != nil || run.Status != agentstore.Running || run.ResultJSON != "{}" {
				t.Fatalf("test incorrectly relied on final result: %+v %v", run, err)
			}
			steps, err := audit.Steps(context.Background(), result.RunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var last agentstore.Step
			for _, step := range steps {
				if step.Kind == "tool" {
					last = step
				}
			}
			if last.Status != agentstore.StepSucceeded {
				t.Fatalf("tool did not finish: %+v", last)
			}
			var refs []string
			switch kind {
			case "evidence":
				if len(result.Evidence) != 1 {
					t.Fatalf("evidence: %+v", result.Evidence)
				}
				refs = []string{result.Evidence[0].ID, result.Evidence[0].BodySHA256}
			case "candidate":
				if len(result.Candidates) != 1 {
					t.Fatalf("candidate: %+v", result.Candidates)
				}
				refs = []string{result.Candidates[0].Reference.ID, result.Candidates[0].Reference.Version}
			case "training_report":
				refs = []string{result.Candidates[0].TrainingReportID, result.Candidates[0].Reference.ID, result.Candidates[0].Reference.Version}
			case "holdout_report":
				refs = []string{result.Candidates[0].HoldoutReportID, result.Candidates[0].Reference.ID, result.Candidates[0].Reference.Version}
			}
			for _, ref := range refs {
				if ref == "" || !reviewJSONContainsString(last.OutputJSON, ref) {
					t.Errorf("completed %s step lost reference %q: %s", kind, ref, last.OutputJSON)
				}
			}
			for _, secret := range []string{"REVIEW_FETCH_BODY", "REVIEW_TRAIN_BODY", "REVIEW_HOLD_BODY", "https://"} {
				if strings.Contains(last.OutputJSON, secret) {
					t.Errorf("step copied raw evidence %q: %s", secret, last.OutputJSON)
				}
			}
		})
	}
}

func TestRunnerReviewProviderUsageOverOutputLimitStopsBeforeTool(t *testing.T) {
	replay := model.NewReplay(model.ReplayStep{Reply: model.Reply{Content: `{"tool":"fetch_page","url":"https://site.example/list"}`, Usage: model.Usage{InputTokens: 10, OutputTokens: 9, Known: true}}})
	cfg, goal, _, source, _ := runnerHarness(t, replay)
	cfg.Budgets.MaxOutputTokens = 8
	result, err := runAgent(t, cfg, goal)
	if !errors.Is(err, crawlagent.ErrBudget) || result.Status != agentstore.Exhausted || source.calls.Load() != 0 || len(replay.Calls()) != 1 {
		t.Fatalf("excess provider output usage continued: %+v %v calls=%d", result, err, len(replay.Calls()))
	}
}
