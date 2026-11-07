package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/pkg/agenteval"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestFixedEvaluationCorpusAndHiddenIsolation(t *testing.T) {
	site := newEvaluationSite()
	defer site.Close()
	seen := map[string]bool{}
	count := 0
	for _, s := range evaluationCorpus() {
		rule := s.Oracle
		rule.ID, rule.Version = "fixture", "v1"
		compiled, err := crawlrule.Compile(rule)
		if err != nil {
			t.Fatal(err)
		}
		for _, split := range []struct {
			name     string
			fixtures []evaluationFixture
		}{{"train", s.Training}, {"holdout", s.Holdout}} {
			if len(split.fixtures) != 3 {
				t.Fatal("corpus count changed")
			}
			scores, err := agenteval.Score(compiled, evaluationFixtures(site.URL, s, split.fixtures))
			if err != nil || scores.Samples != (agenteval.Count{Matched: 3, Total: 3}) || scores.Fields != (agenteval.Count{Matched: 4, Total: 4}) || scores.Negative != (agenteval.Count{Matched: 1, Total: 1}) {
				t.Fatalf("%s/%s: %+v %v", s.ID, split.name, scores, err)
			}
			for _, f := range split.fixtures {
				count++
				hash := evaluationDigest(f.Body)
				if seen[hash] {
					t.Fatal("corpus contains duplicate bodies")
				}
				seen[hash] = true
				res, err := http.Get(site.URL + f.Path)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				want := 200
				if split.name == "holdout" {
					want = 404
				}
				if res.StatusCode != want {
					t.Fatalf("fixture disclosure %s got %d", f.Path, res.StatusCode)
				}
			}
		}
	}
	if count != 18 || corpusDigest() != "020638f035c044d7f8d59f1b46dce99f3390a9ff61cf9a0ff8e988b8e5692f6f" {
		t.Fatalf("fixed corpus changed count=%d hash=%s", count, corpusDigest())
	}
	t.Logf("corpus SHA256: %s", corpusDigest())
}

func TestFixedEvaluationOptionsNeverImplicitlyBill(t *testing.T) {
	for _, o := range []options{{Evaluate: true}, {Evaluate: true, Render: true, RealModel: true}, {Evaluate: true, Render: true, InspectRun: "one"}, {EvalCompare: "x"}, {Evaluate: true, Render: true, InputPrice: 1}} {
		if validateEvaluationOptions(o) == nil {
			t.Fatalf("unsafe options accepted: %+v", o)
		}
	}
	if validateEvaluationOptions(options{Evaluate: true, Render: true, BaseURL: "invalid unused", APIKeyEnv: "unused"}) != nil {
		t.Fatal("Replay depends on model configuration")
	}
	if _, err := evaluationEndpoint("https://user:secret@example.test/v1?key=secret"); err == nil {
		t.Fatal("credential-bearing endpoint accepted")
	}
	path := filepath.Join(t.TempDir(), "partial.json")
	_ = os.WriteFile(path, []byte(`{"report":{}}`), 0600)
	if _, err := readEvaluation(path); err == nil {
		t.Fatal("partial baseline accepted")
	}
}

func TestFixedEvaluationMySQLAndChromium(t *testing.T) {
	dsn := os.Getenv("PAPA_AGENT_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires dedicated agent_test MySQL and sandboxed Chromium")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "agent_test") {
		t.Fatal("dedicated agent_test required")
	}
	bin, ok := launcher.LookPath()
	if !ok {
		t.Fatal("Chromium missing")
	}
	t.Setenv("PAPA_AGENT_DSN", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := command(ctx, &output, options{Evaluate: true, Render: true, Migrate: true, SampleDir: t.TempDir(), BrowserPath: bin}); err != nil {
		t.Fatalf("evaluation: %v\n%s", err, output.String())
	}
	var out evaluationOutput
	if err := json.Unmarshal(output.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if err := out.Report.ValidateComplete(); err != nil {
		t.Fatal(err)
	}
	if len(out.Manifest) != 18 || len(out.Report.Runs) != 9 || len(out.Summary) != 3 {
		t.Fatalf("partial evaluation: %+v", out)
	}
	for _, run := range out.Report.Runs {
		if !run.ReadyForReview || run.InfrastructureFailure || run.Audit.ModelCalls != 7 || run.Audit.Tools.Total != 7 || run.Audit.Tools.Matched != 0 || run.Audit.Tokens != nil || run.EstimatedMicros != nil || run.Scores.Samples.Matched != 3 || run.Scores.Fields.Matched != 4 {
			t.Fatalf("unexpected evaluation: %+v", run)
		}
		// Reopen through read-only command; a deliberately invalid provider
		// config cannot trigger a model call during recovery inspection.
		var view bytes.Buffer
		if err := command(ctx, &view, options{InspectRun: run.RunID, RealModel: true, BaseURL: "invalid"}); err != nil {
			t.Fatal(err)
		}
		var audit auditView
		if json.Unmarshal(view.Bytes(), &audit) != nil || audit.Run.Status != run.Status || len(audit.Steps) != 14 {
			t.Fatal("audit reopen mismatch")
		}
	}
	if _, err := agenteval.Compare(out.Report, out.Report); err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(baselinePath, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var compared bytes.Buffer
	if err := command(ctx, &compared, options{Evaluate: true, Render: true, SampleDir: t.TempDir(), BrowserPath: bin, EvalCompare: baselinePath}); err != nil {
		t.Fatalf("compare command: %v\n%s", err, compared.String())
	}
	var comparison evaluationOutput
	if json.Unmarshal(compared.Bytes(), &comparison) != nil || comparison.Comparison == nil || len(comparison.Report.Runs) != 9 {
		t.Fatal("comparison did not retain all repetitions")
	}
	out.Report.Identity.Budgets.MaxModelCalls++
	bad, _ := json.Marshal(out)
	if err := os.WriteFile(baselinePath, bad, 0600); err != nil {
		t.Fatal(err)
	}
	var rejected bytes.Buffer
	if err := command(ctx, &rejected, options{Evaluate: true, Render: true, SampleDir: t.TempDir(), BrowserPath: bin, EvalCompare: baselinePath}); err == nil {
		t.Fatal("different budget baseline accepted")
	}
	var failure evaluationOutput
	if json.Unmarshal(rejected.Bytes(), &failure) != nil || len(failure.Report.Runs) != 1 || failure.Report.Runs[0].Failure != "incomparable_baseline" || failure.Report.Runs[0].Audit.ModelCalls != 0 {
		t.Fatal("incompatible baseline made calls or continued the suite")
	}
	var broken bytes.Buffer
	if err := command(ctx, &broken, options{Evaluate: true, Render: true, SampleDir: t.TempDir(), BrowserPath: filepath.Join(t.TempDir(), "missing-chromium.exe")}); err == nil {
		t.Fatal("missing renderer passed evaluation")
	}
	var operational evaluationOutput
	if json.Unmarshal(broken.Bytes(), &operational) != nil || operational.Report.ValidateComplete() != nil {
		t.Fatal("failed repetitions disappeared from report")
	}
	for _, run := range operational.Report.Runs {
		if run.Scenario == "rendered-dom" {
			if run.ReadyForReview || !run.InfrastructureFailure || run.Failure != "evaluation_infrastructure_failed" || run.Scores.Samples.Matched != 3 || run.Audit.Tools.Matched != 2 {
				t.Fatalf("offline success masked render failure: %+v", run)
			}
		}
	}
	// Optional artifact export is explicit and confined to the host-selected path.
	if path := os.Getenv("PAPA_EVAL_TEST_REPORT"); path != "" {
		if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordedReplayBaselineContract(t *testing.T) {
	baseline, err := readEvaluation("testdata/d3-replay-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Report.Identity.CorpusSHA256 != corpusDigest() || baseline.Report.Identity.Mode != "replay" || len(baseline.Manifest) != 18 || len(baseline.Report.Runs) != 9 {
		t.Fatal("baseline does not match fixed corpus")
	}
	for _, run := range baseline.Report.Runs {
		if !run.ReadyForReview || run.Audit.Tokens != nil || run.EstimatedMicros != nil {
			t.Fatal("recorded Replay baseline changed meaning")
		}
	}
}
