package crawlagent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
)

func TestRunnerComplexSelectorsNeverCreateCandidates(t *testing.T) {
	replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`, candidateAction(":has(:has(b))"), `{"tool":"inspect_selector","evidence_id":"latest","selector":":has(:has(b))"}`)
	cfg, goal, repo, _, audit := runnerHarness(t, replay)
	cfg.Budgets.MaxModelCalls = 3
	result, err := runAgent(t, cfg, goal)
	if !errors.Is(err, crawlagent.ErrBudget) || len(repo.created) != 0 || len(result.Candidates) != 0 {
		t.Fatalf("unsafe candidate: %+v err=%v created=%v", result, err, repo.created)
	}
	steps, err := audit.Steps(context.Background(), result.RunID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 6 || steps[3].Status != agentstore.StepFailed || steps[5].Status != agentstore.StepFailed {
		t.Fatalf("unsafe tools were not rejected: %+v", steps)
	}
}

func TestRunnerComplexEvidenceRejectedAtEveryIngress(t *testing.T) {
	for _, input := range []string{"fetched", "training", "holdout", "redacted"} {
		t.Run(input, func(t *testing.T) {
			replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`)
			cfg, goal, repo, source, _ := runnerHarness(t, replay)
			cfg.Budgets.MaxModelCalls = 1
			body := strings.Repeat("<section>", 70) + "payload" + strings.Repeat("</section>", 70)
			switch input {
			case "fetched":
				source.body = body
			case "training":
				addRunnerFixture(t, repo, "training", "https://site.example/training", body, "unused")
				goal.Stages[0].TrainingSet = "training"
			case "holdout":
				addRunnerFixture(t, repo, "holdout", "https://site.example/holdout", body, "unused")
				goal.Stages[0].HoldoutSet = "holdout"
			case "redacted":
				cfg.SamplePolicy.RedactBody = func(_ bool, _ []byte) ([]byte, error) { return []byte(body), nil }
			}
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || len(result.Evidence) != 0 || result.Verified {
				t.Fatalf("complex evidence accepted: %+v err=%v", result, err)
			}
			if (input == "training" || input == "holdout") && (len(replay.Calls()) != 0 || source.calls.Load() != 0) {
				t.Fatal("invalid fixture contacted model or website")
			}
		})
	}
}

func TestRunnerHTMLBudgetUsesEvaluationFormatInsideJSON(t *testing.T) {
	// Raw HTML characters are valid inside JSON strings. Marshal would escape
	// them and accidentally test an inert string instead of dual-format input.
	body := `"` + strings.Repeat("<section>", 70) + "payload" + strings.Repeat("</section>", 70) + `"`
	for _, path := range []string{"inspect", "structural", "training", "holdout"} {
		t.Run(path, func(t *testing.T) {
			actions := []string{`{"tool":"fetch_page","url":"seed:0"}`}
			if path == "inspect" {
				actions = append(actions, `{"tool":"inspect_selector","evidence_id":"latest","selector":"section"}`)
			} else {
				actions = append(actions, candidateAction("b"))
				if path == "holdout" {
					actions = append(actions, `{"tool":"finish"}`)
				} else {
					actions = append(actions, `{"tool":"validate_candidate","stage":"catalog"}`)
				}
			}
			replay := replayActions(actions...)
			cfg, goal, repo, source, _ := runnerHarness(t, replay)
			source.body = body
			if path == "training" || path == "holdout" {
				addRunnerFixture(t, repo, path, "https://site.example/"+path, body, "unused")
				if path == "training" {
					goal.Stages[0].TrainingSet = path
				} else {
					goal.Stages[0].HoldoutSet = path
					source.body = "<article><b>PUBLIC</b></article>"
				}
			}
			result, err := runAgent(t, cfg, goal)
			if !errors.Is(err, crawlagent.ErrBudget) || result.Verified || len(replay.Calls()) != len(actions) || len(repo.verifications) != 0 {
				t.Fatalf("HTML in JSON bypassed budget: result=%+v err=%v calls=%d verifications=%d", result, err, len(replay.Calls()), len(repo.verifications))
			}
		})
	}
}
