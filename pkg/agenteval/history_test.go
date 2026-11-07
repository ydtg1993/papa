package agenteval

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

func historyPair() (Report, Report) {
	before := reportFixture()
	before.Identity.HistoryMode = "none"
	before.Identity.HistoryFixtureSHA256 = crawlagent.PromptDigest()
	after := cloneReport(before)
	after.Identity.HistoryMode = "published"
	for i := range after.Runs {
		after.Runs[i].InputBytes = 100
		after.Runs[i].Audit.History = []crawlagent.HistoryRecord{{Stage: "a", Reference: rulestore.Reference{ID: "old", Version: "v1"}, State: "published", RuleSHA256: crawlagent.PromptDigest(), ViewSHA256: crawlagent.PromptDigest(), ViewBytes: 50}}
	}
	return before, after
}

func TestHistoryComparisonAllowsOnlyExplicitTreatment(t *testing.T) {
	before, after := historyPair()
	saved := cloneReport(after)
	comparison, err := CompareHistory(before, after)
	if err != nil || comparison.Before[0].HistoryBytes.Mean != 0 || comparison.After[0].HistoryBytes.Mean != 50 || comparison.After[0].InputBytes.Mean != 100 {
		t.Fatalf("comparison: %+v %v", comparison, err)
	}
	if after.Runs[0].Audit.History[0] != saved.Runs[0].Audit.History[0] {
		t.Fatal("comparison mutated history")
	}
	if _, err := Compare(before, after); err == nil {
		t.Fatal("ordinary comparison silently relaxed")
	}
	for _, mutate := range []func(*Report){
		func(r *Report) { r.Identity.Model = "other" }, func(r *Report) { r.Identity.Budgets.MaxModelCalls++ }, func(r *Report) { r.Identity.HistoryFixtureSHA256 = "different" },
		func(r *Report) { r.Runs[0].Audit.History = nil }, func(r *Report) { r.Runs[0].Audit.History[0].State = "draft" }, func(r *Report) { r.Runs[0].Scores.Fields.Total++ },
		func(r *Report) { r.Runs[0].Audit.History[0].ViewBytes++ }, func(r *Report) { r.Runs[0].InputBytes = -1 },
		func(r *Report) {
			r.Runs[0].Audit.History[0].ViewSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
		},
	} {
		candidate := cloneReport(after)
		mutate(&candidate)
		if _, err := CompareHistory(before, candidate); err == nil {
			t.Fatalf("confounded comparison accepted: %+v", candidate)
		}
	}
	if _, err := CompareHistory(after, before); err == nil {
		t.Fatal("reversed treatment accepted")
	}
}

func TestHistoryFixtureDigestMustBeHex(t *testing.T) {
	before, after := historyPair()
	before.Identity.HistoryFixtureSHA256 = strings.Repeat("z", 64)
	after.Identity.HistoryFixtureSHA256 = before.Identity.HistoryFixtureSHA256
	if _, err := CompareHistory(before, after); err == nil {
		t.Fatal("malformed paired recipe accepted")
	}
}

func TestHistoryComparisonRejectsChangedViewAcrossReports(t *testing.T) {
	_, before := historyPair()
	after := cloneReport(before)
	for i := range after.Runs {
		after.Runs[i].Audit.History[0].ViewSHA256 = strings.Repeat("0", 64)
	}
	if _, err := Compare(before, after); err == nil {
		t.Fatal("different historical views accepted across reports")
	}
}

func TestHistoryMissingAuditCannotHideBehindInfrastructureFailure(t *testing.T) {
	_, after := historyPair()
	after.Runs[0].Audit.History = nil
	after.Runs[0].InfrastructureFailure = true
	after.Runs[0].Audit.ModelCalls = 1
	if err := after.ValidateComplete(); err == nil {
		t.Fatal("model calls without historical preparation accepted")
	}
	// A genuine failure before any model call must remain reportable.
	after.Runs[0].Audit.ModelCalls = 0
	if err := after.ValidateComplete(); err != nil {
		t.Fatal(err)
	}
}
