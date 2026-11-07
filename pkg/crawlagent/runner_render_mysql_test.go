package crawlagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func TestRunnerMySQLChromiumFailureAudit(t *testing.T) {
	for _, cancelRender := range []bool{false, true} {
		name := "render-budget"
		if cancelRender {
			name = "render-canceled"
		}
		t.Run(name, func(t *testing.T) {
			db, repo, blobs, audit, prefix, _ := runnerMySQL(t)
			bin, ok := launcher.LookPath()
			if !ok {
				t.Fatal("real MySQL/Chromium acceptance requested but Chromium is unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				if cancelRender {
					cancel()
					return
				}
				fmt.Fprint(w, `<html><body><script>document.body.textContent='x'.repeat(4096)</script></body></html>`)
			}))
			defer site.Close()
			createRunnerMySQLFixture(t, repo, prefix+"-training", site.URL+"/training", "TRAIN")
			createRunnerMySQLFixture(t, repo, prefix+"-holdout", site.URL+"/hidden", "HIDDEN")
			profile := t.TempDir()
			replay := replayActions(`{"tool":"fetch_page","url":"seed:0","mode":"render"}`)
			a, err := crawlagent.New(crawlagent.Config{
				Model: replay, ModelName: "deterministic-render-failure", Audit: audit, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy,
				Web: webaccess.Config{
					Policy:   webaccess.Policy{Origins: []webaccess.Origin{{URL: site.URL, AllowPrivateNetwork: true}}},
					Renderer: webaccess.RendererConfig{Enabled: true, ExecutablePath: bin, TempDir: profile},
					Budgets:  webaccess.Budgets{MaxResponseBytes: 1024},
				},
				Budgets: crawlagent.Budgets{MaxDuration: 30 * time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := a.Run(ctx, crawlagent.Goal{Site: prefix, Seeds: []string{site.URL + "/render"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-catalog", TrainingSet: prefix + "-training", HoldoutSet: prefix + "-holdout", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}})
			want := agentstore.Exhausted
			if cancelRender {
				want = agentstore.Canceled
			}
			if err == nil || result.Status != want || result.Verified || len(result.Candidates) != 0 || result.Usage.ModelCalls != 1 {
				t.Fatalf("failed render accepted: status=%s verified=%v candidates=%d evidence=%d calls=%d err=%v", result.Status, result.Verified, len(result.Candidates), len(result.Evidence), result.Usage.ModelCalls, err)
			}
			// Approved training evidence survives; a failed live render must not
			// become evidence or expose the independent holdout sample.
			if len(result.Evidence) != 1 || result.Evidence[0].URL != site.URL+"/training" {
				t.Fatalf("failed render changed approved evidence: %+v", result.Evidence)
			}
			// Reopen the audit adapter after Run returns, using a live context:
			// cancellation must not discard the final durable run/step outcomes.
			reopened, err := agentstore.New(db)
			if err != nil {
				t.Fatal(err)
			}
			run, err := reopened.Get(context.Background(), result.RunID)
			if err != nil || run.Status != want || run.FinishedAt == nil {
				t.Fatalf("final audit: status=%s finished=%v err=%v", run.Status, run.FinishedAt, err)
			}
			var stored crawlagent.Result
			if err = json.Unmarshal([]byte(run.ResultJSON), &stored); err != nil || stored.Status != want || stored.Verified {
				t.Fatalf("durable result: %+v err=%v", stored, err)
			}
			steps, err := reopened.Steps(context.Background(), result.RunID, 0)
			if err != nil || len(steps) != 2 || steps[0].Status != agentstore.StepSucceeded || steps[1].Status != agentstore.StepFailed || steps[1].FinishedAt == nil {
				t.Fatalf("durable steps: %+v err=%v", steps, err)
			}
			if _, err = repo.Current(context.Background(), prefix); !errors.Is(err, rulestore.ErrNoRelease) {
				t.Fatalf("failed render changed publication: %v", err)
			}
			entries, err := os.ReadDir(profile)
			if err != nil || len(entries) != 0 {
				t.Fatalf("profile not released: %d err=%v", len(entries), err)
			}
		})
	}
}
