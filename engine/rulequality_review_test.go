package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/rulequality"
)

func TestRuleQualityMissingItemKeyCannotReportHealthy(t *testing.T) {
	report, saves := qualityReviewReport(t, `{"rows":[{"id":"one"}]}`, "url")
	if report.Verdict != rulequality.ProcessingFailure || report.Metrics.FailureCounts[rulequality.FailureRule] != 20 || report.Metrics.ExtractionFailures != 0 || saves != 0 {
		t.Fatalf("item identity contract failure reported as healthy: verdict=%s failures=%v extraction_failures=%d saves=%d", report.Verdict, report.Metrics.FailureCounts, report.Metrics.ExtractionFailures, saves)
	}
}

func TestRuleQualityMalformedJSONIsNotEmptyResult(t *testing.T) {
	for _, tc := range []struct{ name, body, verdict string }{
		{"malformed_json", `{"rows":`, rulequality.SuspectedDrift},
		{"non_array_rows", `{"rows":{}}`, rulequality.SuspectedDrift},
		{"empty_rows", `{"rows":[]}`, rulequality.Partial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, saves := qualityReviewReport(t, tc.body, "id")
			if report.Verdict != tc.verdict || saves != 0 {
				t.Fatalf("parser failure confused with no records: verdict=%s failures=%d want=%s saves=%d", report.Verdict, report.Metrics.ExtractionFailures, tc.verdict, saves)
			}
		})
	}
}

func qualityReviewReport(t *testing.T, body, itemKey string) (rulequality.Report, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	store := rulequality.NewMemoryStore()
	collector, err := rulequality.NewCollector(store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.ItemKey, cfg.Quality = itemKey, collector
	saves := 0
	cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
	f := ruleTestFetcher(t, cfg)
	from := time.Now().Add(-time.Second)
	for i := 0; i < 20; i++ {
		task := ruleTestTask(t, f, RuleRequest{URL: fmt.Sprintf("%s/page-%d", srv.URL, i)})
		if _, err := ruleTestRun(t, f, task); err == nil {
			t.Fatal("expected failed task")
		}
	}
	report, err := rulequality.BuildReport(context.Background(), store, rulequality.Query{
		Scope: rulequality.Scope{Site: cfg.Site, Stage: cfg.Stage, RuleID: "list", Version: "v1"}, From: from, Until: time.Now().Add(time.Second),
	}, rulequality.DefaultPolicy(), "")
	if err != nil {
		t.Fatal(err)
	}
	return report, saves
}
