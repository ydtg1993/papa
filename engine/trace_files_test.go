package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func traceText(t *testing.T, e *Engine, id int) string {
	t.Helper()
	rows, err := e.ListTraceExecution(id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rows)
	return string(b)
}
func TestTraceFileRetentionPolicy(t *testing.T) {
	for _, mode := range []string{"success", "error", "panic", "warn", "fail", "manual"} {
		t.Run(mode, func(t *testing.T) {
			e, pool := traceEngine(t, true)
			task := &Task{ID: 7, Site: "synthetic", Stage: "stub", URL: "https://example.test"}
			if mode == "manual" {
				if err := e.TraceNextExecution(7); err != nil {
					t.Fatal(err)
				}
			}
			f := &tracePolicyFetcher{mode: mode}
			func() {
				defer func() {
					r := recover()
					if (r != nil) != (mode == "panic") {
						t.Fatalf("panic=%v", r)
					}
				}()
				_ = e.runAttempt(context.Background(), f, task, 0)
			}()
			rows, err := e.ListTraceExecution(7)
			if err != nil {
				t.Fatal(err)
			}
			if (len(rows) > 0) != (mode != "success") {
				t.Fatalf("%s rows=%+v", mode, rows)
			}
			if len(rows) > 0 && !strings.Contains(rows[0].Data, "value") {
				t.Fatalf("context lost: %+v", rows)
			}
			if strings.Contains(pool.all(), "task_trace") {
				t.Fatal("trace issued SQL")
			}
			if mode == "success" {
				entries, err := os.ReadDir(e.cfg.Crawler.TraceFiles.Dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("clean success created files: %v %v", entries, err)
				}
			}
		})
	}
}

type tracePolicyFetcher struct {
	mode   string
	cancel context.CancelFunc
}

func (f *tracePolicyFetcher) GetStage() string { return "stub" }
func (f *tracePolicyFetcher) FetchHandler(ctx context.Context, t *Task, e *Engine) error {
	t.Trace.Step("before", map[string]string{"value": "<html>"})
	switch f.mode {
	case "error":
		return errors.New("failure")
	case "panic":
		panic("failure")
	case "warn":
		t.Trace.Warn("warning", nil, nil)
	case "fail":
		t.Trace.Fail("explicit", nil, nil)
	case "cancelwarn":
		t.Trace.Warn("warning", nil, nil)
		f.cancel()
		return ctx.Err()
	case "cancel":
		f.cancel()
		return ctx.Err()
	case "deadline":
		return context.DeadlineExceeded
	}
	return nil
}
func TestTraceCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "cancelwarn", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := traceEngine(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = e.runAttempt(ctx, &tracePolicyFetcher{mode: mode, cancel: cancel}, &Task{ID: 7}, 0)
			rows, err := e.ListTraceExecution(7)
			if err != nil || (len(rows) > 0) != (mode != "cancel") {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
		})
	}
}
func TestTraceBoundedConcurrentTail(t *testing.T) {
	tr := newTrace(nil, 7, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tr.Step(strings.Repeat("x", 3000), map[string]string{"large": strings.Repeat("a", 100000)})
			}
		}()
	}
	wg.Wait()
	tr.Fail("final", errors.New("boom"), nil)
	rows := tr.flush()
	if len(rows) > 500 || !tr.truncated || rows[len(rows)-1].Step != "final" {
		t.Fatalf("tail=%d truncated=%v", len(rows), tr.truncated)
	}
	size := 0
	for _, s := range rows {
		size += traceStepBytes(s)
	}
	if size > maxTraceBufferBytes {
		t.Fatalf("bytes=%d", size)
	}
	if tr.flush() != nil {
		t.Fatal("not idempotent")
	}
}
func TestTraceDataBoundedCyclesAndUnsupported(t *testing.T) {
	m := map[string]any{}
	m["cycle"] = m
	s, cut := traceData(m)
	if !cut || len(s) > maxTraceDataBytes || !json.Valid([]byte(s)) {
		t.Fatalf("cycle=%s %v", s, cut)
	}
	s, _ = traceData(make(chan int))
	if !strings.Contains(s, "_marshal_error") {
		t.Fatal(s)
	}
	s, _ = traceData(map[string]string{"html": "<b>"})
	if strings.Contains(s, `\u003c`) {
		t.Fatal(s)
	}
}
func TestTraceManualSelectionOneShotRetryAndCapacity(t *testing.T) {
	e, _ := traceEngine(t, true)
	if err := e.TraceNextExecution(7); err != nil {
		t.Fatal(err)
	}
	ctx := e.selectTraceExecution(withEvidenceExecution(context.Background()), 7)
	for i := 0; i < 2; i++ {
		if err := e.runAttempt(ctx, &tracePolicyFetcher{}, &Task{ID: 7}, i); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := e.ListTraceExecution(7)
	if len(rows) != 2 || rows[0].ExecutionID != rows[1].ExecutionID {
		t.Fatalf("retry rows=%+v", rows)
	}
	if e.selectTraceExecution(context.Background(), 7).Value(traceSelectionKey{}).(bool) {
		t.Fatal("selection reused")
	}
	s := e.traceService()
	s.mu.Lock()
	s.armed[8] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if e.selectTraceExecution(context.Background(), 8).Value(traceSelectionKey{}).(bool) {
		t.Fatal("expired selection")
	}
	for i := 1; i <= maxTraceSelections; i++ {
		if err := e.TraceNextExecution(i); err != nil {
			t.Fatal(err)
		}
	}
	if e.TraceNextExecution(maxTraceSelections+1) == nil {
		t.Fatal("unbounded selections")
	}
}
func TestTraceFailedRetryAndLaterCleanExecutionPreserveEvidence(t *testing.T) {
	e, _ := traceEngine(t, true)
	ctx := withEvidenceExecution(context.Background())
	_ = e.runAttempt(ctx, &tracePolicyFetcher{mode: "error"}, &Task{ID: 7}, 0)
	_ = e.runAttempt(ctx, &tracePolicyFetcher{}, &Task{ID: 7}, 1)
	before := traceText(t, e, 7)
	_ = e.runAttempt(context.Background(), &tracePolicyFetcher{}, &Task{ID: 7}, 0)
	if got := traceText(t, e, 7); got != before || !strings.Contains(got, "failure") {
		t.Fatalf("lost failure: %s", got)
	}
}
func TestTracePersistenceFailureDoesNotChangeOutcome(t *testing.T) {
	e, _ := traceEngine(t, true)
	e.cfg.Crawler.TraceFiles.Dir = fmt.Sprintf("%s/file", t.TempDir())
	if err := os.WriteFile(e.cfg.Crawler.TraceFiles.Dir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.runAttempt(context.Background(), &tracePolicyFetcher{mode: "warn"}, &Task{ID: 7}, 0); err != nil {
		t.Fatal(err)
	}
}
func TestTraceNilReceiver(t *testing.T) {
	var tr *Trace
	tr.Step("x", nil)
	tr.Warn("x", nil, nil)
	tr.Fail("x", nil, nil)
	tr.setResult(nil)
	tr.finish()
	if tr.flush() != nil {
		t.Fatal("nil trace")
	}
}

func TestTraceActualDeadlineAndManualEmpty(t *testing.T) {
	e, _ := traceEngine(t, true)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_ = e.runAttempt(ctx, &tracePolicyFetcher{mode: "deadline"}, &Task{ID: 7}, 0)
	rows, _ := e.ListTraceExecution(7)
	if len(rows) == 0 {
		t.Fatal("actual deadline discarded")
	}
	tr := newTrace(e.traceService(), 8, 0)
	tr.identityHash = strings.Repeat("a", 64)
	tr.manual = true
	tr.setResult(nil)
	tr.finish()
	rows, _ = e.ListTraceExecution(8)
	if len(rows) != 1 || rows[0].Step != "执行完成" {
		t.Fatalf("manual empty=%+v", rows)
	}
}
func TestTraceFileCleanupAndRetention(t *testing.T) {
	e, _ := traceEngine(t, true)
	if e.traceRetention() != 7*24*time.Hour {
		t.Fatal("default retention")
	}
	e.cfg.Crawler.Trace.Retention = -1
	if e.traceRetention() != 0 {
		t.Fatal("negative retention")
	}
	e.cfg.Crawler.Trace.Retention = time.Hour
	tr := newTrace(e.traceService(), 7, 0)
	tr.identityHash = strings.Repeat("a", 64)
	tr.Fail("failure", nil, nil)
	tr.finish()
	e.cleanupTrace(-time.Hour)
	rows, _ := e.ListTraceExecution(7)
	if len(rows) == 0 {
		t.Fatal("negative cleanup removed files")
	}
	if err := e.traceService().files.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, _ = e.ListTraceExecution(7)
	if len(rows) != 0 {
		t.Fatal("expired files retained")
	}
}
