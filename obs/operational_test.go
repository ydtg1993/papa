package obs

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

type operationalSection struct{}

func (operationalSection) Key() string   { return "framework" }
func (operationalSection) Title() string { return "test" }
func (operationalSection) Build(context.Context, *Input) (*SectionResult, error) {
	return &SectionResult{Checks: []Check{
		NewCheck("unfinished_tasks", SeverityFail, false, "old backlog", ""),
		NewCheck("crawl_fail_rate", SeverityWarn, false, "actual failed requests", ""),
	}}, nil
}

func TestHistoricalSnapshotDoesNotFailOnCurrentBacklog(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	rep, err := Generate(context.Background(), Options{Now: now, Run: RunMeta{To: now.Add(-24 * time.Hour)}, Sections: []Section{operationalSection{}}, Mode: ModeSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != VerdictWarn || !rep.Checks[0].Skipped || rep.Checks[1].Skipped {
		t.Fatalf("historical state polluted verdict or hid real failure: %+v", rep)
	}
}

func TestObservedRuntimeDoesNotInventCalendarHours(t *testing.T) {
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	evs := []events.Event{{RunID: "a", TS: day.Add(12 * time.Hour)}, {RunID: "a", TS: day.Add(12*time.Hour + 5*time.Minute)},
		{RunID: "b", TS: day.Add(13 * time.Hour)}, {RunID: "b", TS: day.Add(13*time.Hour + 3*time.Minute)}}
	if got := ObservedRuntime(evs, day, day.Add(24*time.Hour)); got != 8*time.Minute {
		t.Fatalf("runtime=%s", got)
	}
}

func TestProblemSeverityParticipatesInOperationalVerdict(t *testing.T) {
	for _, sev := range []Severity{SeverityWarn, SeverityFail} {
		rep := &Report{Verdict: VerdictPass, Checks: []Check{NewCheck("healthy", SeverityWarn, true, "", "")}, Problems: []Problem{{ID: "breaker", Title: "异常熔断", Severity: sev}}}
		applyVerdictPolicy(rep)
		want := VerdictWarn
		if sev == SeverityFail {
			want = VerdictFail
		}
		if rep.Verdict != want || rep.VerdictReason != "发现异常：异常熔断" {
			t.Fatalf("problem hidden: %+v", rep)
		}
	}
}
