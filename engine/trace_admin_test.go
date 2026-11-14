package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/tracestore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListTraceDisabled(t *testing.T) {
	e, _ := traceEngine(t, false)
	if _, err := e.ListTrace(7); err == nil {
		t.Fatal("disabled trace readable")
	}
}
func TestListTraceEmpty(t *testing.T) {
	e, _ := traceEngine(t, true)
	rows, err := e.ListTrace(7)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty: %+v %v", rows, err)
	}
}
func TestTraceFilesSurviveRestartAndSeparateExecutions(t *testing.T) {
	e, pool := traceEngine(t, true)
	task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "u"}
	for attempt := 0; attempt < 2; attempt++ {
		tr := e.newTrace(task, attempt)
		tr.Fail("retry", errors.New("fixture"), nil)
		tr.finish()
	}
	rows, err := e.ListTrace(7)
	if err != nil || len(rows) != 2 || rows[1].Attempt != 1 {
		t.Fatalf("retry: %+v %v", rows, err)
	}
	fresh, _ := traceEngine(t, true)
	fresh.cfg.Crawler.TraceFiles.Dir = e.cfg.Crawler.TraceFiles.Dir
	rows, err = fresh.ListTrace(7)
	if err != nil || len(rows) != 2 {
		t.Fatalf("restart: %+v %v", rows, err)
	}
	tr := e.newTrace(task, 0)
	tr.Step("new round", nil)
	tr.finish()
	rows, err = e.ListTrace(7)
	if err != nil || len(rows) != 1 || rows[0].Step != "new round" {
		t.Fatalf("mixed rounds: %+v %v", rows, err)
	}
	if pool.all() != "" {
		t.Fatal("trace used SQL")
	}
}
func TestTraceCleanupAndCorruptionUseFilesOnly(t *testing.T) {
	e, pool := traceEngine(t, true)
	store := e.traceService().files
	for i, at := range []time.Time{time.Now().Add(-2 * time.Hour), time.Now()} {
		if err := store.Write(context.Background(), tracestore.Batch{TaskID: i + 1, IdentityHash: "id", ExecutionID: "round", RecordedAt: at, Steps: []core.TraceStep{{Step: "fixture"}}}); err != nil {
			t.Fatal(err)
		}
	}
	e.cleanupTrace(time.Hour)
	old, err := e.ListTrace(1)
	if err != nil || len(old) != 0 {
		t.Fatalf("expired: %+v %v", old, err)
	}
	live, err := e.ListTrace(2)
	if err != nil || len(live) != 1 {
		t.Fatalf("retained: %+v %v", live, err)
	}
	if err := filepath.WalkDir(e.cfg.Crawler.TraceFiles.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			return os.WriteFile(path, []byte("broken"), 0600)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ListTrace(2); err == nil {
		t.Fatal("corrupt latest file silently accepted")
	}
	if pool.all() != "" {
		t.Fatal("cleanup queried or wrote SQL")
	}
}
func TestTraceBoundsAndQuotaDoNotChangeTaskOutcome(t *testing.T) {
	e, pool := traceEngine(t, true)
	e.cfg.Crawler.TraceFiles.MaxFiles = 1
	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "u"}
	tr := e.newTrace(task, 0)
	for i := 0; i < 600; i++ {
		tr.Step("step", nil)
	}
	tr.finish()
	rows, err := e.ListTraceExecution(7)
	if err != nil || len(rows) != 500 || !rows[0].Truncated {
		t.Fatalf("bounded tail: %d %v", len(rows), err)
	}
	if err := e.runAttempt(context.Background(), &stubFetcher{steps: []string{"quota"}}, task, 0); err != nil {
		t.Fatalf("trace quota failed business: %v", err)
	}
	if pool.all() != "" {
		t.Fatal("quota fell back to DB")
	}
}
func TestStopWaitsForTracePublication(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	cfg := &config.Config{}
	cfg.Crawler.Trace.Enabled = true
	cfg.Crawler.TraceFiles.Dir = t.TempDir()
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	entered := make(chan struct{})
	fetcher := &traceStopFetcher{entered: entered}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 1}, fetcher, nil)
	e.ApplyRegisterStage()
	task := &Task{Stage: "stub", Site: "stub", URL: "https://fixture.invalid/stop"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("not drained")
	}
	rows, err := e.ListTrace(task.ID)
	if err != nil || len(rows) < 2 {
		t.Fatalf("stop evidence lost: %+v %v", rows, err)
	}
	if strings.Contains(f.written(), "crawler_task_trace") {
		t.Fatal("trace issued SQL")
	}
}

type traceStopFetcher struct{ entered chan struct{} }

func (f *traceStopFetcher) GetStage() string { return "stub" }
func (f *traceStopFetcher) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	task.Trace.Step("before stop", nil)
	close(f.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestEmptyTraceRoundReplacesPreviousEvidence(t *testing.T) {
	e, _ := traceEngine(t, true)
	task := &Task{ID: 7, Site: "s", Stage: "s", URL: strings.Repeat("u", 1000)}
	tr := e.newTrace(task, 0)
	tr.Fail("old failure", errors.New("old"), nil)
	tr.finish()
	tr = e.newTrace(task, 0)
	tr.setResult(nil)
	tr.finish()
	rows, err := e.ListTrace(7)
	if err != nil || len(rows) != 1 || rows[0].Step != "执行完成" {
		t.Fatalf("stale evidence: %+v %v", rows, err)
	}
}
