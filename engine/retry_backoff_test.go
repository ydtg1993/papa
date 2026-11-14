package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestHandlerRetryBackoffPolicy(t *testing.T) {
	for _, tc := range []struct {
		base    time.Duration
		attempt int
		bound   time.Duration
	}{
		{time.Second, 1, time.Second}, {time.Second, 2, 2 * time.Second},
		{time.Second, 8, time.Minute}, {time.Hour, 1, time.Minute},
		{math.MaxInt64, math.MaxInt, time.Minute}, {0, 3, 0}, {-1, 3, 0},
	} {
		for range 64 {
			if got := retryBackoff(tc.base, tc.attempt); got < tc.bound-tc.bound/2 || got > tc.bound {
				t.Fatalf("%+v: %v", tc, got)
			}
		}
	}
}

func TestJitteredRetriesKeepOneClaimAndExactCounts(t *testing.T) {
	e, f, task, out := persistenceFixture(t)
	info := e.stages["stub:stub"]
	info.config.MaxAttempts, info.config.Backoff = 3, 2*time.Millisecond
	calls := 0
	info.fetcher = stageRunVersionFetcher(func(_ context.Context, task *Task, _ *Engine) error {
		calls++
		if task.Retry != calls-1 {
			t.Errorf("handler %d saw retry %d", calls, task.Retry)
		}
		if calls < 3 {
			return errors.New("synthetic transient failure")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.stageHandler(info)(ctx, task); err != nil {
		t.Fatal(err)
	}
	drainTestEvents(t, e)
	if calls != 3 || task.Retry != 2 || len(claimStatements(f)) != 1 || strings.Count(f.written(), "`retry`=retry +") != 2 || countTaskEvents(out, events.TypeTaskRetry) != 2 || countTaskEvents(out, events.TypeTaskFinished) != 1 || countTaskEvents(out, events.TypeTaskFailed) != 0 {
		t.Fatalf("calls=%d retry=%d SQL=%s events=%v", calls, task.Retry, f.written(), out.snapshot())
	}
}

func TestJitteredRetryCancellationDoesNotIncrementOrSettle(t *testing.T) {
	e, f, task, out := persistenceFixture(t)
	info := e.stages["stub:stub"]
	info.config.MaxAttempts, info.config.Backoff = 3, time.Duration(math.MaxInt64)
	fetcher := &recordingFetcher{stage: "stub", err: errors.New("synthetic failure")}
	info.fetcher = fetcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.stageHandler(info)(ctx, task) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("handler leaked")
		}
	})
	fetcher.waitCallsWithin(t, 1, 5*time.Second)
	// A saturated retry must not spin into a second attempt before cancellation.
	select {
	case err := <-done:
		done <- err
		t.Fatalf("retry exited early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		done <- err // leave completion available for failure-safe cleanup
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	drainTestEvents(t, e)
	if len(fetcher.calls()) != 1 || task.Retry != 0 || len(releaseStatements(f)) != 1 || strings.Contains(f.written(), "`retry`=retry +") || countTaskEvents(out, events.TypeTaskRetry) != 0 {
		t.Fatalf("cancel retried: %s", f.written())
	}
	assertNoTerminalStatusWrite(t, f)
}

func TestStopInterruptsSaturatedWorkerBackoff(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)
	fetcher := &recordingFetcher{stage: "stub", err: errors.New("synthetic failure")}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 3, WorkerCount: 1, QueueSize: 8, Backoff: time.Duration(math.MaxInt64)}, fetcher, nil)
	e.ApplyRegisterStage()
	t.Cleanup(func() { e.Stop(5 * time.Second) })
	task := &Task{Stage: "stub", Site: "stub", URL: "https://fixture.invalid/retry"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	fetcher.waitCallsWithin(t, 1, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	_, stats := e.Stop(3 * time.Second)
	pool := e.pools.Get(stubKey)
	if pool == nil {
		t.Fatal("missing started pool")
	}
	if drained, inFlight := pool.Stop(time.Second); !drained || inFlight != 0 {
		t.Fatalf("worker failed to exit: %v %d", drained, inFlight)
	}
	if stats[stubKey].Unfinished != 0 || task.Retry != 0 || len(fetcher.calls()) != 1 || len(releaseStatements(f)) != 1 || strings.Contains(f.written(), "`retry`=retry +") {
		t.Fatalf("Stop retry regression: stats=%v retry=%d SQL=%s", stats, task.Retry, f.written())
	}
	assertNoTerminalStatusWrite(t, f)
}
