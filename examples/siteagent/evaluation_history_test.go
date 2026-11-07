package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/pkg/agenteval"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
)

func TestHistoryEvaluationOptionsAndRecipe(t *testing.T) {
	for _, o := range []options{{EvalHistory: "published"}, {Evaluate: true, Render: true, EvalHistory: "auto"}, {Evaluate: true, Render: true, EvalHistory: "failed"}} {
		if validateEvaluationOptions(o) == nil {
			t.Fatal("unexpected implicit history mode")
		}
	}
	for _, mode := range []string{"", "none", "published"} {
		if validateEvaluationOptions(options{Evaluate: true, Render: true, EvalHistory: mode}) != nil {
			t.Fatal("valid history treatment rejected")
		}
	}
	if len(evaluationHistoryDigest()) != 64 {
		t.Fatal("history recipe identity missing")
	}
}

func TestRecordedHistoryComparisonContract(t *testing.T) {
	control, err := readEvaluation("testdata/d4-replay-control.json")
	if err != nil {
		t.Fatal(err)
	}
	history, err := readEvaluation("testdata/d4-replay-history.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agenteval.CompareHistory(control.Report, history.Report); err != nil {
		t.Fatal(err)
	}
	if control.Report.Identity.CorpusSHA256 != corpusDigest() || control.Report.Identity.HistoryFixtureSHA256 != evaluationHistoryDigest() || control.Report.Identity.Mode != "replay" || len(control.Manifest) != 18 || len(history.Manifest) != 18 || len(history.Report.Runs) != 9 {
		t.Fatal("recorded history corpus changed")
	}
	for i, run := range history.Report.Runs {
		before := control.Report.Runs[i]
		if !run.ReadyForReview || !before.ReadyForReview || run.Scores != before.Scores || run.Audit.ModelCalls != 7 || before.Audit.ModelCalls != 7 || run.Audit.Corrections != 0 || before.Audit.Corrections != 0 || run.InputBytes <= before.InputBytes || run.Audit.Tokens != nil || run.EstimatedMicros != nil || before.Audit.Tokens != nil || before.EstimatedMicros != nil {
			t.Fatal("recorded Replay comparison changed meaning")
		}
	}
}

func TestHistoryEvaluationMySQLAndChromium(t *testing.T) {
	dsn := os.Getenv("PAPA_AGENT_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires dedicated agent_test MySQL and sandboxed Chromium")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "agent_test") {
		t.Fatal("dedicated agent_test database required")
	}
	bin, ok := launcher.LookPath()
	if !ok {
		t.Fatal("Chromium missing")
	}
	t.Setenv("PAPA_AGENT_DSN", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var control, treated bytes.Buffer
	if err := command(ctx, &control, options{Evaluate: true, Render: true, EvalHistory: "none", Migrate: true, SampleDir: t.TempDir(), BrowserPath: bin}); err != nil {
		t.Fatalf("control: %v\n%s", err, control.String())
	}
	path := filepath.Join(t.TempDir(), "control.json")
	if err := os.WriteFile(path, control.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(ctx, &treated, options{Evaluate: true, Render: true, EvalHistory: "published", EvalCompare: path, SampleDir: t.TempDir(), BrowserPath: bin}); err != nil {
		t.Fatalf("history: %v\n%s", err, treated.String())
	}
	var before, after evaluationOutput
	if json.Unmarshal(control.Bytes(), &before) != nil || json.Unmarshal(treated.Bytes(), &after) != nil || after.Comparison == nil {
		t.Fatal("history comparison missing")
	}
	if len(before.Report.Runs) != 9 || len(after.Report.Runs) != 9 || before.Report.Identity.PromptSHA256 != crawlagent.PromptDigest() || !agenteval.ComparableHistoryIdentity(before.Report.Identity, after.Report.Identity) {
		t.Fatal("paired protocol differs")
	}
	for i, run := range after.Report.Runs {
		if !run.ReadyForReview || run.Audit.ModelCalls != before.Report.Runs[i].Audit.ModelCalls || run.Audit.Corrections != 0 || run.Audit.Tokens != nil || run.EstimatedMicros != nil || len(run.Audit.History) != 1 || len(before.Report.Runs[i].Audit.History) != 0 || run.Scores != before.Report.Runs[i].Scores || run.InputBytes <= before.Report.Runs[i].InputBytes {
			t.Fatalf("paired Replay result invalid: %+v", run)
		}
		var out bytes.Buffer
		if err := command(ctx, &out, options{InspectRun: run.RunID, RealModel: true, BaseURL: "invalid"}); err != nil {
			t.Fatal(err)
		}
		var view auditView
		if json.Unmarshal(out.Bytes(), &view) != nil || len(view.Steps) != 15 || view.Steps[0].Kind != "history" {
			t.Fatal("persisted history audit not recoverable")
		}
		var plan crawlagent.Plan
		if json.Unmarshal([]byte(view.Run.SnapshotJSON), &plan) != nil || !plan.HistoryEnabled {
			t.Fatal("persisted snapshot lost disclosure intent")
		}
		if strings.Contains(view.Steps[0].OutputJSON, "<article") || strings.Contains(view.Steps[0].OutputJSON, "Saffron") {
			t.Fatal("history audit duplicated hidden/body data")
		}
	}
}
