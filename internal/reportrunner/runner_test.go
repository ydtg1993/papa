package reportrunner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func closeRunner(t *testing.T, r *Runner) {
	t.Helper()
	// Test teardown/file assertions are not the production shutdown SLA.
	// The blocked-writer test below separately enforces a 20ms close deadline.
	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestBlockedWriterCloseBudgetAndNoReplacement(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true, OnExit: true, Interval: time.Millisecond}})
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r.write = func(string, *obs.Report) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil, errors.New("fixture")
	}
	r.Start()
	<-entered
	for i := 0; i < 10000; i++ {
		r.Emit(events.Event{Type: events.TypeTaskFinished})
		r.Periodic()
	}
	ctx, c := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer c()
	start := time.Now()
	err := r.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("close budget: %v %s", err, time.Since(start))
	}
	r.Start()
	r.Periodic()
	if calls.Load() != 1 {
		t.Fatal("replacement writer")
	}
	close(release)
	closeRunner(t, r)
}
func TestSignalCountsAndCurrentDay(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true, OnExit: true}})
	results := make(chan *obs.Report, 1)
	r.write = func(_ string, rep *obs.Report) ([]string, error) {
		results <- rep
		return nil, errors.New("skip prune")
	}
	r.Start()
	for _, kind := range []string{events.TypeTaskFinished, events.TypeTaskFailed, events.TypeTaskRetry, events.TypeRunStopped} {
		r.Emit(events.Event{Type: kind, TS: time.Now().Add(-48 * time.Hour), Counts: map[string]int{"remaining": 2}})
	}
	closeRunner(t, r)
	rep := <-results
	m := rep.Sections[0].Metrics.(map[string]any)
	c := m["day"].(counters)
	if c.Received != 4 || c.Finished != 1 || c.Failed != 1 || c.Retry != 1 || rep.Verdict != obs.VerdictUnknown || rep.Engine.Source != "signals" || !rep.Inputs.SnapshotAt.IsZero() {
		t.Fatalf("bad report %+v", rep)
	}
	if m["last_run_stop"].(*events.Event).Counts["remaining"] != 2 {
		t.Fatal("lost stop fact")
	}
	a := aggregate{}
	old := time.Now().Add(-48 * time.Hour)
	r.accept(&a, envelope{events.Event{Type: events.TypeTaskFinished}, old})
	r.snapshot(&a, time.Now())
	if a.counts.Received != 0 || a.run.Received != 1 {
		t.Fatal("day rollover retains daily history")
	}
}
func TestFiniteDimensionsAndActualQueueBudget(t *testing.T) {
	r := NewWithLimits(Options{}, inputbudget.Limits{MaxEvents: 2, MaxDedupKeys: 3, MaxRetainedBytes: 64 << 10})
	if cap(r.incoming) != 1 || cap(r.jobs) != 1 {
		t.Fatal("budget ignored")
	}
	a := aggregate{}
	for i := 0; i < 10000; i++ {
		r.accept(&a, envelope{events.Event{Site: string(rune(i))}, time.Now()})
	}
	if len(a.groups) != 3 || a.overflow != 9997 {
		t.Fatalf("unbounded groups %d overflow %d", len(a.groups), a.overflow)
	}
}
func TestWriterPanicIsolated(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true, OnExit: true, Interval: time.Millisecond}})
	var calls atomic.Int32
	entered := make(chan struct{})
	r.write = func(string, *obs.Report) ([]string, error) { calls.Add(1); close(entered); panic("fixture") }
	r.Start()
	<-entered
	closeRunner(t, r)
	if calls.Load() != 1 || r.failures.Load() != 1 {
		t.Fatal("panic writer replaced or not recorded")
	}
}
func TestConcurrentEmitRequestClose(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true, OnExit: true}})
	r.write = func(string, *obs.Report) ([]string, error) { return nil, errors.New("fixture") }
	r.Start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.Emit(events.Event{Type: events.TypeTaskFinished})
				r.Periodic()
			}
		}()
	}
	closeRunner(t, r)
	wg.Wait()
	r.Start()
	closeRunner(t, r)
}
func TestStaticInputsDetachedAndNoCallbacks(t *testing.T) {
	in := obs.Inputs{Thresholds: map[string]string{"x": "before"}}
	r := New(Options{Inputs: in})
	in.Thresholds["x"] = "after"
	if r.options.Inputs.Thresholds["x"] != "before" {
		t.Fatal("borrowed input")
	}
	typ := reflect.TypeOf(Options{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() == reflect.Func || f.Name == "DB" || f.Name == "Logger" || f.Name == "Sections" || f.Name == "Patterns" || f.Name == "EngineState" {
			t.Fatal("online callback/resource", f.Name)
		}
	}
	r.Start()
	closeRunner(t, r)
	r.Exit()
}
func TestUnknownReportArtifactsAndPruneOwnFilesOnly(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, time.Now().Add(-30*24*time.Hour).Format("2006-01-02"))
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"report.json": "old", "keep.txt": "keep"} {
		if err := os.WriteFile(filepath.Join(old, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := New(Options{Config: Config{Enabled: true, OnExit: true, ReportDir: dir}})
	r.Start()
	closeRunner(t, r)
	for _, name := range []string{"report.json", "report.md", "ai-brief.json", "ai-brief.md"} {
		if _, err := os.Stat(filepath.Join(dir, time.Now().Format("2006-01-02"), name)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, time.Now().Format("2006-01-02"), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep obs.Report
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Verdict != obs.VerdictUnknown {
		t.Fatalf("online artifact must be valid and unknown: verdict=%s err=%v", rep.Verdict, err)
	}
	if _, err := os.Stat(filepath.Join(old, "keep.txt")); err != nil {
		t.Fatal("removed unrelated file", err)
	}
	if _, err := os.Stat(filepath.Join(old, "report.json")); !os.IsNotExist(err) {
		t.Fatal("did not prune artifact")
	}
}
func TestTruncatedSignalsVisible(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true}})
	r.Start()
	r.Emit(events.Event{Type: events.TypeTaskFinished, URL: strings.Repeat("x", 10000)})
	closeRunner(t, r)
	if r.truncated.Load() != 1 {
		t.Fatal("truncation not visible")
	}
}

func TestFloodDropsWithoutConsumer(t *testing.T) {
	r := NewWithLimits(Options{Config: Config{Enabled: true}}, inputbudget.Limits{MaxEvents: 2})
	// Hold the collector absent to model complete downstream starvation.
	r.started = true
	for i := 0; i < 10000; i++ {
		r.Emit(events.Event{Type: events.TypeTaskFinished})
	}
	if len(r.incoming) != 2 || r.dropped.Load() != 9998 {
		t.Fatalf("queue=%d dropped=%d", len(r.incoming), r.dropped.Load())
	}
}

func TestGaugesFailuresLossAndSnapshotOwnership(t *testing.T) {
	r := NewWithLimits(Options{}, inputbudget.Limits{MaxDedupKeys: 2})
	a := aggregate{}
	now := time.Now()
	for i := 0; i < 30; i++ {
		r.accept(&a, envelope{events.Event{Type: events.TypeTaskFailed, Error: "synthetic failure", Stages: map[string]events.StageSnapshot{"a": {Workers: i}}, Queues: map[string]events.QueueSnapshot{"q": {Backlog: int64(i)}}}, now})
	}
	r.accept(&a, envelope{events.Event{Type: "observation_loss", Counts: map[string]int{"dropped": 3, "truncated": 4}}, now})
	rep := r.snapshot(&a, now)
	m := rep.Sections[0].Metrics.(map[string]any)
	if len(a.samples) != 16 || a.stages["a"].Workers != 29 || a.queues["q"].Backlog != 29 || m["upstream_dropped"].(uint64) != 3 || len(rep.Problems) != 2 {
		t.Fatalf("incomplete snapshot: %+v", m)
	}
	r.accept(&a, envelope{events.Event{Stages: map[string]events.StageSnapshot{"a": {Workers: 999}}}, now})
	if m["last_stage_gauges"].(map[string]events.StageSnapshot)["a"].Workers != 29 {
		t.Fatal("snapshot aliases collector")
	}
	r.enqueue(rep)
	r.enqueue(r.snapshot(&a, now))
	if r.coalesced.Load() != 1 {
		t.Fatal("coalescing invisible")
	}
}

// These microbenchmarks compare producer admission only. They are not an SLA
// or an end-to-end crawler/production throughput claim.
func BenchmarkSignalAdmission(b *testing.B) {
	for _, mode := range []string{"off", "on", "blocked_writer"} {
		b.Run(mode, func(b *testing.B) {
			r := New(Options{Config: Config{Enabled: mode != "off", Retention: -1}})
			release := make(chan struct{})
			entered := make(chan struct{})
			var once sync.Once
			r.write = func(string, *obs.Report) ([]string, error) {
				once.Do(func() { close(entered) })
				if mode == "blocked_writer" {
					<-release
				}
				return nil, nil
			}
			r.Start()
			if mode == "blocked_writer" {
				r.Periodic()
				<-entered
			}
			ev := events.Event{Type: events.TypeTaskFinished, Site: "synthetic", Stage: "fetch"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Emit(ev)
			}
			b.StopTimer()
			close(release)
			ctx, c := context.WithTimeout(context.Background(), time.Second)
			defer c()
			if err := r.Close(ctx); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func TestArchiveLossSeparateFromSignalLoss(t *testing.T) {
	r := New(Options{})
	a := aggregate{}
	now := time.Now()
	r.accept(&a, envelope{events.Event{Type: "observation_loss", Counts: map[string]int{"archive_dropped": 7}}, now})
	rep := r.snapshot(&a, now)
	m := rep.Sections[0].Metrics.(map[string]any)
	if m["archive_dropped"].(uint64) != 7 || m["upstream_dropped"].(uint64) != 0 {
		t.Fatal("archive and ingress mixed")
	}
	if len(rep.Problems) != 1 || rep.Problems[0].ID != "archive.loss" {
		t.Fatalf("wrong quality problem %+v", rep.Problems)
	}
}
func TestStatusRemainsReadableAfterWriterFailure(t *testing.T) {
	r := New(Options{Config: Config{Enabled: true, OnExit: true}})
	r.write = func(string, *obs.Report) ([]string, error) { return nil, errors.New("synthetic I/O failure") }
	r.Start()
	r.Emit(events.Event{URL: strings.Repeat("x", 10000)})
	closeRunner(t, r)
	got := r.Status()
	if got.Failures != 1 || got.Truncated != 1 {
		t.Fatalf("status %+v", got)
	}
}
func TestRetentionCursorAdvancesBeyondUnrelatedEntries(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		name := filepath.Join(dir, "0000-unrelated-"+strconv.Itoa(i))
		if err := os.WriteFile(name, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// More expired directories than one batch, each kept alive by an unrelated
	// file: the old restart-at-first-batch implementation fails regardless of
	// filesystem enumeration order (not only on lexicographically ordered NTFS).
	var artifacts, keepFiles []string
	for i := 0; i < 170; i++ {
		old := filepath.Join(dir, time.Date(2000, 1, 1+i, 0, 0, 0, 0, time.Local).Format("2006-01-02"))
		if err := os.Mkdir(old, 0700); err != nil {
			t.Fatal(err)
		}
		artifact, keep := filepath.Join(old, "report.json"), filepath.Join(old, "unrelated.txt")
		for _, p := range []string{artifact, keep} {
			if err := os.WriteFile(p, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		artifacts, keepFiles = append(artifacts, artifact), append(keepFiles, keep)
	}
	r := New(Options{Config: Config{ReportDir: dir}})
	defer r.pruning.close()
	for i := 0; i < 4; i++ {
		if err := r.prune(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, artifact := range artifacts {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatal("later expired artifact starved", artifact, err)
		}
	}
	for _, keep := range keepFiles {
		if _, err := os.Stat(keep); err != nil {
			t.Fatal("unrelated file in date directory removed", keep, err)
		}
	}
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(filepath.Join(dir, "0000-unrelated-"+strconv.Itoa(i))); err != nil {
			t.Fatal("unrelated file removed", err)
		}
	}
}
