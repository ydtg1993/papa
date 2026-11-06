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

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	q "github.com/ydtg1993/papa/v3/pkg/rulequality"
)

func TestRuleQualityExampleRequiresDedicatedInputs(t *testing.T) {
	for _, dsn := range []string{"", "root@tcp(localhost:3306)/business", "invalid"} {
		t.Setenv("PAPA_QUALITY_DSN", dsn)
		if err := command(context.Background(), &bytes.Buffer{}, options{DataDir: t.TempDir()}); err == nil {
			t.Fatalf("unsafe or invalid DSN accepted: %q", dsn)
		}
	}
	t.Setenv("PAPA_QUALITY_DSN", "root@tcp(localhost:1)/quality_test")
	for _, opts := range []options{{}, {InspectRequest: "request", Migrate: true}} {
		if err := command(context.Background(), &bytes.Buffer{}, opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
}

func TestRuleQualityReplayCandidateUsesHostOwnedIdentity(t *testing.T) {
	steps := replaySteps()
	if len(steps) != 4 {
		t.Fatalf("replay calls=%d", len(steps))
	}
	var action struct {
		Tool string                     `json:"tool"`
		Rule map[string]json.RawMessage `json:"rule"`
	}
	if err := json.Unmarshal([]byte(steps[1].Reply.Content), &action); err != nil {
		t.Fatal(err)
	}
	if action.Tool != "submit_candidate" {
		t.Fatal("missing candidate action")
	}
	if _, ok := action.Rule["id"]; ok {
		t.Fatal("model chose a rule ID")
	}
	if _, ok := action.Rule["version"]; ok {
		t.Fatal("model chose a version")
	}
	for _, step := range steps {
		if strings.Contains(step.Reply.Content, "fetch_page") || strings.Contains(step.Reply.Content, "publish") {
			t.Fatal("replay must use retained evidence without fetching or publishing")
		}
	}
	old, err := crawlrule.Compile(exampleRule("rule", "old", ".title"))
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := crawlrule.Compile(exampleRule("rule", "fixed", ".title,.heading"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://127.0.0.1/missing")
	response := &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`<article data-id="one"><h2 class="heading">Known title</h2></article>`)}
	oldResult, err := old.Evaluate(response)
	if err != nil || len(oldResult.Records) != 1 {
		t.Fatalf("missing optional field should still succeed: %v", err)
	}
	if _, exists := oldResult.Records[0]["title"]; exists {
		t.Fatal("old selector unexpectedly matched")
	}
	newResult, err := fixed.Evaluate(response)
	if err != nil || newResult.Records[0]["title"] != "Known title" {
		t.Fatalf("fixed candidate mismatch: %+v %v", newResult, err)
	}
}

func TestRuleQualityExampleRealMySQLWorkflow(t *testing.T) {
	dsn := os.Getenv("PAPA_QUALITY_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_QUALITY_EXAMPLE_TEST_DSN to a disposable quality_test database; real MySQL workflow was not run")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "quality_test") {
		t.Fatal("test database name must contain quality_test")
	}
	t.Setenv("PAPA_QUALITY_DSN", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := command(ctx, &output, options{DataDir: t.TempDir(), Migrate: true}); err != nil {
		t.Fatalf("workflow: %v\n%s", err, output.String())
	}
	var result demoResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Mode != "deterministic-replay" || len(result.Scenarios) != 5 || !result.Verified || !result.ReleaseUnchanged || !result.ReopenedRequest || !result.ReplayRejected || !result.UnrelatedReportsRejected || result.ReplayCalls != 4 || result.MissingEvidenceStatus != q.NeedsEvidence || result.MissingEvidenceModelCalls != 0 || result.QualityFailures != 1 || result.SaveCallsDuringQualityFailure != 1 {
		t.Fatalf("workflow invariants: %+v", result)
	}
	if result.Request.Status != q.AwaitingReview || result.Run.Status != agentstore.AwaitingReview || result.Run.RunID != result.Request.RunID || result.Candidate.Version == "v1" || len(result.Run.Candidates) != 1 {
		t.Fatalf("candidate/run: %+v", result)
	}
	candidate := result.Run.Candidates[0]
	if !candidate.TrainingPassed || !candidate.HoldoutPassed || candidate.TrainingReportID == "" || candidate.HoldoutReportID == "" || candidate.TrainingReportID == candidate.HoldoutReportID {
		t.Fatalf("independent acceptance absent: %+v", candidate)
	}
	for _, scenario := range result.Scenarios {
		if scenario.Report.Query.Scope.ReleaseID != result.InitialRelease.ID || scenario.Report.Query.Scope.Version != "v1" || len(scenario.Report.Evidence) == 0 || scenario.Report.Evidence[0].EvidenceID == "" {
			t.Fatalf("missing real pinned snapshot: %+v", scenario)
		}
		switch scenario.Name {
		case "normal":
			if scenario.Report.Verdict != q.ObservedOK || scenario.SuccessfulTasks != 20 {
				t.Fatalf("normal: %+v", scenario)
			}
		case "missing":
			if scenario.Report.Verdict != q.SuspectedDrift || scenario.SuccessfulTasks != 20 || scenario.FailedTasks != 0 || scenario.Report.BaselineID == "" || scenario.Report.Metrics.ExtractionFailures != 0 {
				t.Fatalf("silent field loss: %+v", scenario)
			}
		case "rate":
			if scenario.Report.Verdict != q.UpstreamFailure || scenario.FailedTasks != 20 || scenario.Report.Metrics.FailureCounts[q.FailureRateLimit] != 20 {
				t.Fatalf("rate: %+v", scenario)
			}
		case "duplicates":
			if scenario.Report.Verdict != q.ObservedOK || scenario.NormalStops != 20 || scenario.SuccessfulTasks != 40 || scenario.Report.Metrics.RawRows != 40 || scenario.Report.Metrics.NewRows != 20 {
				t.Fatalf("duplicates: %+v", scenario)
			}
		case "save":
			if scenario.Report.Verdict != q.ProcessingFailure || scenario.FailedTasks != 20 || scenario.Report.Metrics.FailureCounts[q.FailureSave] != 20 {
				t.Fatalf("save: %+v", scenario)
			}
		}
	}
	var first, second bytes.Buffer
	opts := options{InspectRequest: result.Request.ID}
	if err := command(ctx, &first, opts); err != nil {
		t.Fatal(err)
	}
	if err := command(ctx, &second, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("read-only reopening changed persisted state")
	}
	var view inspection
	if err := json.Unmarshal(first.Bytes(), &view); err != nil || view.Run.ID != result.Run.RunID || view.Report.ID != result.Request.ReportID {
		t.Fatalf("read-only inspection: %+v %v", view, err)
	}
}
