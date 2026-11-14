package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
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
		seen := map[time.Duration]bool{}
		for range 64 {
			got := retryBackoff(tc.base, tc.attempt)
			seen[got] = true
			if got < tc.bound-tc.bound/2 || got > tc.bound {
				t.Fatalf("%+v: %v", tc, got)
			}
		}
		if tc.bound > 1 && len(seen) < 2 {
			t.Fatal("Handler policy did not jitter")
		}
	}
}

func TestJitteredRetriesKeepOneClaimAndExactCounts(t *testing.T) {
	e, f, task, out := persistenceFixture(t)
	t.Cleanup(func() { drainTestEvents(t, e) })
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
	synctest.Test(t, func(t *testing.T) {
		e, f, task, out := persistenceFixture(t)
		t.Cleanup(func() { drainTestEvents(t, e) })
		e.cfg.Crawler.Trace.Mode = config.TraceOff
		info := e.stages["stub:stub"]
		info.config.MaxAttempts, info.config.Backoff = 3, time.Duration(math.MaxInt64)
		fetcher := &recordingFetcher{stage: "stub", err: errors.New("synthetic failure")}
		info.fetcher = fetcher
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var runErr error
		go func() { defer close(done); runErr = e.stageHandler(info)(ctx, task) }()
		t.Cleanup(func() {
			cancel()
			synctest.Wait()
			select {
			case <-done:
			default:
				time.Sleep(time.Minute + time.Second)
				synctest.Wait()
			}
		})
		synctest.Wait()
		if len(fetcher.calls()) != 1 || !strings.Contains(f.written(), "`last_finished_at`") {
			t.Fatalf("first attempt not finished: %s", f.written())
		}
		select {
		case <-done:
			t.Fatalf("retry exited instead of waiting: %v", runErr)
		default:
		}
		if task.Retry != 0 {
			t.Fatal("retry counted before waiting")
		}
		// All other goroutines are durably blocked after the completed attempt;
		// no wall-clock sleep or in-flight request can masquerade as backoff.
		before := time.Now()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("backoff did not unblock on cancellation")
		}
		if !errors.Is(runErr, context.Canceled) || !time.Now().Equal(before) {
			t.Fatalf("cancel err=%v elapsed=%v", runErr, time.Since(before))
		}
		drainTestEvents(t, e)
		if len(fetcher.calls()) != 1 || task.Retry != 0 || len(releaseStatements(f)) != 1 || strings.Contains(f.written(), "`retry`=retry +") || countTaskEvents(out, events.TypeTaskClaimed) != 1 || countTaskEvents(out, events.TypeTaskRetry) != 0 || countTaskEvents(out, events.TypeTaskFailed) != 0 || countTaskEvents(out, events.TypeTaskFinished) != 0 {
			t.Fatalf("cancel retried/settled: SQL=%s events=%v", f.written(), out.snapshot())
		}
		assertNoTerminalStatusWrite(t, f)
	})
}

func TestStopInterruptsSaturatedWorkerBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeTaskDB()
		f.noRows = true
		e := newTestEngine(t, f)
		fetcher := &recordingFetcher{stage: "stub", err: errors.New("synthetic failure")}
		e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 3, WorkerCount: 1, QueueSize: 8, Backoff: time.Duration(math.MaxInt64)}, fetcher, nil)
		e.ApplyRegisterStage()
		t.Cleanup(func() { e.Stop(5 * time.Second); time.Sleep(time.Minute + time.Second); synctest.Wait() })
		task := &Task{Stage: "stub", Site: "stub", URL: "https://fixture.invalid/retry"}
		if err := e.SubmitTask(task); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(fetcher.calls()) != 1 || !strings.Contains(f.written(), "`last_finished_at`") || task.Retry != 0 {
			t.Fatalf("not waiting between attempts: %s", f.written())
		}
		pool := e.pools.Get(stubKey)
		if pool == nil {
			t.Fatal("missing started pool")
		}
		_, _, _, active, _ := pool.Stats()
		if active != 1 {
			t.Fatalf("not an active worker: %d", active)
		}
		// The fake driver does not execute SQL. Explicitly model the pending row
		// retained by Release, instead of letting its default COUNT=0 claim a drain.
		f.mu.Lock()
		f.count = 1
		f.mu.Unlock()
		before := time.Now()
		drained, stats := e.Stop(3 * time.Second)
		synctest.Wait()
		if drained || len(stats) != 1 || stats[stubKey].Queued != 1 || stats[stubKey].Unfinished != 0 || !time.Now().Equal(before) {
			t.Fatalf("Stop lost pending work or waited for backoff: drained=%v stats=%v elapsed=%v", drained, stats, time.Since(before))
		}
		_, _, _, active, queued := pool.Stats()
		stops := 0
		for {
			select {
			case ev := <-pool.Activities():
				if ev.Type == workerpool.ActivityWorkerStop {
					stops++
				}
			default:
				goto checked
			}
		}
	checked:
		if active != 0 || queued != 0 || stops != 1 || e.SchedulerResidentCount() != 0 {
			t.Fatalf("worker not drained: active=%d queued=%d stops=%d resident=%d", active, queued, stops, e.SchedulerResidentCount())
		}
		if task.Retry != 0 || len(fetcher.calls()) != 1 || len(releaseStatements(f)) != 1 || strings.Contains(f.written(), "`retry`=retry +") {
			t.Fatalf("Stop retry regression: %s", f.written())
		}
		assertNoTerminalStatusWrite(t, f)
	})
}

func TestHandlerRetryTimingUsesPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, f, task, out := persistenceFixture(t)
		t.Cleanup(func() { drainTestEvents(t, e) })
		e.cfg.Crawler.Trace.Mode = config.TraceOff
		info := e.stages["stub:stub"]
		info.config.MaxAttempts = 3
		info.config.Backoff = 3 * time.Second
		boom := errors.New("synthetic failure")
		var at []time.Time
		info.fetcher = stageRunVersionFetcher(func(context.Context, *Task, *Engine) error { at = append(at, time.Now()); return boom })
		start := time.Now()
		err := e.stageHandler(info)(context.Background(), task)
		synctest.Wait()
		if !errors.Is(err, boom) || len(at) != 3 || task.Retry != 2 || len(claimStatements(f)) != 1 {
			t.Fatalf("err=%v calls=%d retry=%d", err, len(at), task.Retry)
		}
		if !at[0].Equal(start) || !time.Now().Equal(at[2]) {
			t.Fatal("wait before first or after final attempt")
		}
		for i := 1; i < 3; i++ {
			bound := time.Duration(i) * 3 * time.Second
			gap := at[i].Sub(at[i-1])
			if gap < bound/2 || gap > bound {
				t.Fatalf("retry %d gap=%v", i, gap)
			}
		}
		if countTaskEvents(out, events.TypeTaskRetry) != 2 || countTaskEvents(out, events.TypeTaskFailed) != 1 || countTaskEvents(out, events.TypeTaskFinished) != 0 {
			t.Fatal(out.snapshot())
		}
	})
}

// Cancellation can win immediately after the retry timer was selected. When
// it is already visible before IncrementRetry starts, no write was attempted
// and the claim must not be classified as an ambiguous additive write.
func TestRetryCancelledBeforeIncrementReleasesClaim(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[persistent], func(t *testing.T) {
			e, f, task, out := persistenceFixture(t)
			t.Cleanup(func() { drainTestEvents(t, e) })
			if persistent {
				claimed, _, err := e.claimForExecution(context.Background(), task)
				if err != nil || !claimed {
					t.Fatalf("claim %v %v", claimed, err)
				}
			} else {
				task.ID = 0
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := e.incrementExecutionRetry(ctx, task)
			if !errors.Is(err, context.Canceled) || task.Retry != 0 || strings.Contains(f.written(), "`retry`=retry +") || len(e.execution.Snapshot()) != 0 {
				t.Fatalf("cancel before retry write: err=%v retry=%d unsettled=%v SQL=%s", err, task.Retry, e.execution.Snapshot(), f.written())
			}
			wantRelease := 0
			if persistent {
				wantRelease = 1
			}
			if len(releaseStatements(f)) != wantRelease {
				t.Fatalf("release count %d want %d", len(releaseStatements(f)), wantRelease)
			}
			assertNoTerminalStatusWrite(t, f)
			if countTaskEvents(out, events.TypeTaskRetry) != 0 {
				t.Fatal("cancelled retry announced")
			}
		})
	}
}

func TestRetryCancellationDuringWriteRemainsUnconfirmed(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-started", true: "lost-ack"}[afterCommit], func(t *testing.T) {
			e, f, task, out := persistenceFixture(t)
			t.Cleanup(func() { drainTestEvents(t, e) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			inject := func(tx *gorm.DB) {
				if values, ok := tx.Statement.Dest.(map[string]any); ok {
					if _, retry := values["retry"]; retry {
						writes++
						cancel()
						tx.AddError(context.Canceled)
					}
				}
			}
			var registerErr error
			if afterCommit {
				registerErr = e.db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:retry_cancel_ack", inject)
			} else {
				registerErr = e.db.Callback().Update().Before("gorm:update").Register("test:retry_cancel_start", inject)
			}
			if registerErr != nil {
				t.Fatal(registerErr)
			}
			err := e.incrementExecutionRetry(ctx, task)
			if !errors.Is(err, context.Canceled) || writes != 1 || task.Retry != 0 || len(e.execution.Snapshot()) != 1 || len(releaseStatements(f)) != 0 {
				t.Fatalf("err=%v writes=%d retry=%d unsettled=%v SQL=%s", err, writes, task.Retry, e.execution.Snapshot(), f.written())
			}
			if committed := strings.Contains(f.written(), "`retry`=retry +"); committed != afterCommit {
				t.Fatalf("driver reached=%v afterCommit=%v", committed, afterCommit)
			}
			if countTaskEvents(out, events.TypeTaskRetry) != 0 {
				t.Fatal("ambiguous retry announced")
			}
			assertNoTerminalStatusWrite(t, f)
		})
	}
}
