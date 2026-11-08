package crawlagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func TestRunnerMySQLChromiumWaitReopen(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "ready-candidate"
		if timeout {
			name = "missing-timeout"
		}
		t.Run(name, func(t *testing.T) {
			db, repo, blobs, audit, prefix, dir := runnerMySQL(t)
			bin, ok := launcher.LookPath()
			if !ok {
				t.Fatal("real MySQL/Chromium acceptance requires Chromium")
			}
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><body><article>loading</article><script>setTimeout(()=>{document.querySelector('article').innerHTML='<b id="ready">LIVE_READY</b>'},800)</script></body></html>`)
			}))
			defer site.Close()
			createRunnerMySQLFixture(t, repo, prefix+"-training", site.URL+"/training", "TRAIN")
			createRunnerMySQLFixture(t, repo, prefix+"-holdout", site.URL+"/hidden", "HIDDEN")
			selector, timeoutMS := "#ready", 3000
			if timeout {
				selector, timeoutMS = "#never", 100
			}
			action := runnerJSON(map[string]any{"tool": "fetch_page", "url": "seed:0", "mode": "render", "wait": map[string]any{"selector": selector, "state": "visible", "timeout_ms": timeoutMS}})
			actions := []string{action}
			if !timeout {
				actions = append(actions, candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`, `{"tool":"finish"}`)
			}
			cfg := crawlagent.Config{Model: replayActions(actions...), ModelName: "deterministic-wait-replay", Audit: audit, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy,
				Web:     webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: site.URL, AllowPrivateNetwork: true}}}, Renderer: webaccess.RendererConfig{Enabled: true, ExecutablePath: bin, TempDir: t.TempDir()}},
				Budgets: crawlagent.Budgets{MaxDuration: 30 * time.Second, MaxModelCalls: len(actions)}}
			goal := crawlagent.Goal{Site: prefix, Seeds: []string{site.URL + "/render"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-catalog", TrainingSet: prefix + "-training", HoldoutSet: prefix + "-holdout", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
			result, err := runAgent(t, cfg, goal)
			want := agentstore.AwaitingReview
			if timeout {
				want = agentstore.Exhausted
			}
			if result.Status != want || result.Verified == timeout || (timeout && err == nil) || (!timeout && err != nil) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if timeout {
				if len(result.Evidence) != 1 || len(result.Candidates) != 0 {
					t.Fatalf("failed render retained live evidence/candidate: %+v", result)
				}
			} else {
				if len(result.Evidence) != 2 || len(result.Candidates) != 1 {
					t.Fatalf("missing live evidence/candidate: %+v", result)
				}
				live := result.Evidence[1]
				if live.Origin != "live_page" || result.Evidence[0].Origin != "host_sample" {
					t.Fatal("live and fixture provenance mixed")
				}
				sample, err := repo.Sample(context.Background(), live.ID)
				if err != nil {
					t.Fatal(err)
				}
				body, err := blobs.Get(context.Background(), sample.BodySHA256, rulestore.MaxSampleBodyBytes)
				if err != nil || !sample.Rendered || !strings.Contains(string(body), `>LIVE_READY</b>`) {
					t.Fatalf("wrong final DOM: err=%v body=%s", err, body)
				}
			}
			// Reopen adapters and file handles after completion, never replay Run.
			reopened, err := agentstore.New(db)
			if err != nil {
				t.Fatal(err)
			}
			run, err := reopened.Get(context.Background(), result.RunID)
			if err != nil || run.Status != want || run.FinishedAt == nil {
				t.Fatalf("durable status: %+v err=%v", run, err)
			}
			var stored crawlagent.Result
			if err = json.Unmarshal([]byte(run.ResultJSON), &stored); err != nil || stored.Verified != result.Verified || len(stored.Evidence) != len(result.Evidence) {
				t.Fatalf("durable result: %+v err=%v", stored, err)
			}
			for i := range stored.Evidence {
				if stored.Evidence[i].Origin != result.Evidence[i].Origin {
					t.Fatal("provenance lost after reopen")
				}
			}
			steps, err := reopened.Steps(context.Background(), result.RunID, 0)
			if err != nil || len(steps) != 2*len(actions) {
				t.Fatalf("steps=%d err=%v", len(steps), err)
			}
			outcome := "succeeded"
			if timeout {
				outcome = "render_wait_timeout"
				if steps[1].Status != agentstore.StepFailed {
					t.Fatalf("timeout not failed: %+v", steps[1])
				}
			}
			if !strings.Contains(steps[1].OutputJSON, `"outcome":"`+outcome+`"`) || !strings.Contains(steps[1].InputJSON, `"selector_sha256"`) {
				t.Fatalf("durable wait summary: %+v", steps[1])
			}
			reopenedBlobs, err := rulestore.NewFileBlobs(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopenedBlobs.Close()
			reopenedRepo, err := rulestore.New(db, reopenedBlobs)
			if err != nil {
				t.Fatal(err)
			}
			if !timeout {
				v, err := reopenedRepo.Get(context.Background(), result.Candidates[0].Reference)
				if err != nil || v.EverPublished || v.State != "verified" {
					t.Fatalf("candidate after reopen: %+v err=%v", v, err)
				}
			}
			if _, err = reopenedRepo.Current(context.Background(), prefix); !errors.Is(err, rulestore.ErrNoRelease) {
				t.Fatalf("publication changed: %v", err)
			}
		})
	}
}
