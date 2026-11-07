package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestReplayDefaultNeverConstructsRealProvider(t *testing.T) {
	// Deliberately unusable real configuration proves that its validation and
	// credential lookup are not on the default replay path.
	opts := options{BaseURL: "not-a-valid-endpoint", Model: "", APIKeyEnv: ""}
	fixtures := stageFixtures("http://127.0.0.1:12345", true)
	steps := replaySteps("http://127.0.0.1:12345", fixtures, true)
	provider, name, err := selectProvider(opts, steps)
	if err != nil || name != "deterministic-replay" {
		t.Fatalf("provider=%T name=%q error=%v", provider, name, err)
	}
	if _, ok := provider.(*model.Replay); !ok || len(steps) != 18 {
		t.Fatalf("provider=%T decisions=%d", provider, len(steps))
	}
	for _, step := range steps {
		var action map[string]json.RawMessage
		if err := json.Unmarshal([]byte(step.Reply.Content), &action); err != nil {
			t.Fatal(err)
		}
		if raw, exists := action["rule"]; exists {
			var rule map[string]json.RawMessage
			if err := json.Unmarshal(raw, &rule); err != nil {
				t.Fatal(err)
			}
			if _, exists := rule["id"]; exists {
				t.Fatal("replay supplied a host-owned rule ID")
			}
			if _, exists := rule["version"]; exists {
				t.Fatal("replay supplied a host-owned rule version")
			}
		}
	}
}

func TestHostFixturesHaveIndependentBodiesAndExpectedPagination(t *testing.T) {
	for _, fixture := range stageFixtures("http://127.0.0.1:12345", true) {
		t.Run(fixture.goal.Stage, func(t *testing.T) {
			if fixture.training.URL == fixture.holdout.URL || bytes.Equal(fixture.training.Body, fixture.holdout.Body) {
				t.Fatal("training and holdout overlap")
			}
			rule := fixture.rule
			rule.ID, rule.Version = "host-fixture-check", "v1"
			compiled, err := crawlrule.Compile(rule)
			if err != nil {
				t.Fatal(err)
			}
			trainURL, _ := url.Parse(fixture.training.URL)
			holdoutURL, _ := url.Parse(fixture.holdout.URL)
			report, err := compiled.Verify([]crawlrule.Fixture{
				{Name: "train", Response: &htmlfetch.Response{URL: trainURL, StatusCode: fixture.training.StatusCode, Body: fixture.training.Body}, Rendered: fixture.training.Rendered, Expect: fixture.trainingWant},
				{Name: "holdout", Response: &htmlfetch.Response{URL: holdoutURL, StatusCode: fixture.holdout.StatusCode, Body: fixture.holdout.Body}, Rendered: fixture.holdout.Rendered, Expect: fixture.holdoutWant},
			})
			if err != nil || !report.Passed {
				t.Fatalf("fixture contract failed: %+v %v", report, err)
			}
		})
	}
}

func TestCommandRequiresExplicitInputsAndStoppedWorker(t *testing.T) {
	t.Setenv("PAPA_AGENT_DSN", "")
	for _, opts := range []options{{}, {InspectRun: "one", InterruptRun: "two"}, {InterruptRun: "stopped"}} {
		if err := command(context.Background(), &bytes.Buffer{}, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
}

func TestSiteAgentExampleMySQLAndChromium(t *testing.T) {
	dsn := os.Getenv("PAPA_AGENT_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_AGENT_EXAMPLE_TEST_DSN to a dedicated disposable agent_test database; real MySQL/Chromium integration was not run")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "agent_test") {
		t.Fatal("integration test requires a dedicated agent_test database")
	}
	bin, ok := launcher.LookPath()
	if !ok {
		t.Fatal("PAPA_AGENT_EXAMPLE_TEST_DSN was supplied but installed Chromium is unavailable")
	}
	t.Setenv("PAPA_AGENT_DSN", dsn)
	for _, render := range []bool{false, true} {
		name := "http"
		if render {
			name = "controlled_render_and_record"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var out bytes.Buffer
			if err := command(ctx, &out, options{SampleDir: t.TempDir(), Migrate: true, Render: render, BrowserPath: bin}); err != nil {
				t.Fatalf("example: %v\noutput: %s", err, out.String())
			}
			var result demoResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			count, calls := 3, 14
			if render {
				count, calls = 4, 18
			}
			if result.Mode != "replay" || result.Status != agentstore.AwaitingReview || !result.Verified || !result.Unpublished || len(result.Candidates) != count || result.RestoredCandidates != count || result.RestoredAuditSteps != calls*2 || result.Usage.ModelCalls != calls || !result.PendingBecameUnknown {
				t.Fatalf("unexpected analysis: %+v", result)
			}
			for _, candidate := range result.Candidates {
				if !candidate.TrainingPassed || !candidate.HoldoutPassed || candidate.TrainingReportID == "" || candidate.HoldoutReportID == "" || candidate.TrainingReportID == candidate.HoldoutReportID {
					t.Fatalf("independent reports missing: %+v", candidate)
				}
			}
			// Inspection is an independent command and cannot initialize a real
			// provider, migrate, or execute the persisted run again.
			var first, second bytes.Buffer
			inspectOpts := options{InspectRun: result.RunID, RealModel: true, BaseURL: "invalid", APIKeyEnv: ""}
			if err := command(ctx, &first, inspectOpts); err != nil {
				t.Fatal(err)
			}
			if err := command(ctx, &second, inspectOpts); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Fatal("read-only inspection changed the persisted run")
			}
			var view auditView
			if err := json.Unmarshal(first.Bytes(), &view); err != nil || view.Run.ID != result.RunID || len(view.Steps) != calls*2 {
				t.Fatalf("restored inspection: %+v %v", view, err)
			}
			var interrupted bytes.Buffer
			if err := command(ctx, &interrupted, options{InspectRun: result.InterruptionExampleID}); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(interrupted.Bytes(), &view); err != nil || view.Run.Status != agentstore.Interrupted || len(view.Steps) != 1 || view.Steps[0].Status != agentstore.StepUnknown {
				t.Fatalf("interrupted audit: %+v %v", view, err)
			}
		})
	}
}
