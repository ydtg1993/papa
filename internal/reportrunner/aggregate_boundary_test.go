package reportrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestFailureSampleBudgetDoesNotBreakCredentialRedaction(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{Config: Config{Enabled: true, OnExit: true, ReportDir: dir}})
	r.Start()
	r.Emit(events.Event{Type: events.TypeTaskFailed, Site: "fixture", Stage: "fetch", Error: "https://user:SECRET" + strings.Repeat("x", 300) + "@example.test/"})
	r.Emit(events.Event{Type: events.TypeTaskFailed, Site: "fixture", Stage: "fetch", Error: "plain failure"})
	closeRunner(t, r)
	if r.Status().Truncated != 1 {
		t.Fatal("sample omission missing from quality counters")
	}
	for _, name := range []string{"report.json", "report.md", "ai-brief.json", "ai-brief.md"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*", name))
		if err != nil || len(paths) != 1 {
			t.Fatalf("expected one %s artifact: %v %v", name, paths, err)
		}
		data, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "SECRET") {
			t.Fatalf("failure sample shortening leaked credential into %s", name)
		}
		if name == "report.json" && !strings.Contains(string(data), "plain failure") {
			t.Fatal("short failure evidence was lost")
		}
	}
}

func TestFailureSampleOmissionDoesNotDoubleCountTruncatedSignal(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true}})
	r.Start()
	r.Emit(events.Event{Type: events.TypeTaskFailed, Truncated: true, Error: strings.Repeat("x", 300)})
	closeRunner(t, r)
	if r.Status().Truncated != 1 {
		t.Fatal("one signal counted twice for transport and sample omission")
	}
}

func TestMidnightBacklogDoesNotRollDailyBucketBackward(t *testing.T) {
	r := New(Options{})
	a := aggregate{}
	midnight := time.Date(2026, 11, 14, 0, 0, 0, 0, time.Local)
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFinished}, midnight.Add(-time.Second)})
	r.snapshot(&a, midnight.Add(time.Second))
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFinished}, midnight.Add(2 * time.Second)})
	// Concurrent admission and midnight backlog can deliver an earlier day late.
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFailed}, midnight.Add(-time.Millisecond)})
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFinished}, midnight.Add(3 * time.Second)})
	if a.counts.Finished != 2 || a.counts.Failed != 0 || a.run.Received != 4 {
		t.Fatalf("daily bucket lost or mixed signals: day=%+v run=%+v", a.counts, a.run)
	}
	rep := r.snapshot(&a, midnight.Add(4*time.Second))
	if a.late != 1 || len(rep.Problems) != 1 || rep.Problems[0].ID != "signals.loss" {
		t.Fatalf("late daily exclusion is silent: late=%d problems=%+v", a.late, rep.Problems)
	}
}

func TestSameDayBacklogExpandsReportWindow(t *testing.T) {
	r := New(Options{})
	a := aggregate{}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	r.snapshot(&a, now)
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFinished}, now.Add(-time.Minute)})
	rep := r.snapshot(&a, now.Add(time.Minute))
	if !rep.Inputs.WindowFrom.Equal(now.Add(-time.Minute)) {
		t.Fatalf("window excludes counted signal: %+v", rep.Inputs)
	}
}

func TestLongAndTruncatedDimensionsNeverMerge(t *testing.T) {
	r := New(Options{})
	a := aggregate{}
	now := time.Now()
	for _, suffix := range []string{"a", "b"} {
		r.accept(&a, envelope{events.Event{Site: strings.Repeat("x", 256) + suffix, Stage: "fetch"}, now})
	}
	r.accept(&a, envelope{events.Event{Site: "cut", Stage: "fetch", Truncated: true}, now})
	if len(a.groups) != 0 || a.overflow != 3 || a.counts.Received != 3 {
		t.Fatalf("uncertain dimensions were counted as exact: groups=%+v overflow=%d", a.groups, a.overflow)
	}
}
