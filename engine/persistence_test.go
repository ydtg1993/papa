package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
)

func persistenceFixture(t *testing.T) (*Engine, *fakeTaskDB, *Task, *collectorEmitter) {
	t.Helper()
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	c := &collectorEmitter{}
	e.SetEventsEmitter(c)
	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}
	e.dedupCache.Add(task.Unique())
	return e, f, task, c
}

func failStatusWrites(t *testing.T, e *Engine, status models.TaskStatus, failures int) *int {
	t.Helper()
	calls := new(int)
	if err := e.db.Callback().Update().Before("gorm:update").Register("test:persistence-failure", func(tx *gorm.DB) {
		if values, ok := tx.Statement.Dest.(map[string]any); ok && values["status"] == status {
			*calls++
			if failures < 0 || *calls <= failures {
				tx.AddError(errors.New("test: transient state write outage"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	return calls
}

func countTaskEvents(c *collectorEmitter, kind string) int {
	n := 0
	for _, ev := range c.snapshot() {
		if ev.Type == kind {
			n++
		}
	}
	return n
}

func TestClaimFailureDoesNotExecuteAndCanCancel(t *testing.T) {
	e, _, task, c := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	failStatusWrites(t, e, models.TaskStatusProcessing, -1)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	err := e.stageHandler(e.stages["stub:stub"])(ctx, task)
	if !errors.Is(err, context.DeadlineExceeded) || len(fetcher.calls()) != 0 || countTaskEvents(c, events.TypeTaskClaimed) != 0 {
		t.Fatalf("unconfirmed claim executed: err=%v calls=%d events=%v", err, len(fetcher.calls()), c.snapshot())
	}
}

func TestClaimRetriesBeforeExecutingOnce(t *testing.T) {
	e, _, task, c := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	calls := failStatusWrites(t, e, models.TaskStatusProcessing, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.stageHandler(e.stages["stub:stub"])(ctx, task); err != nil {
		t.Fatal(err)
	}
	drainTestEvents(t, e)
	if *calls != 3 || len(fetcher.calls()) != 1 || countTaskEvents(c, events.TypeTaskFinished) != 1 {
		t.Fatalf("claim attempts=%d fetch calls=%d events=%v", *calls, len(fetcher.calls()), c.snapshot())
	}
}

func TestTerminalWriteRetriesWithoutReplayingBusiness(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			e, _, task, c := persistenceFixture(t)
			fetcher := &recordingFetcher{stage: "stub"}
			status := models.TaskStatusSuccess
			if fail {
				status = models.TaskStatusFailed
				fetcher.err = core.WrapNoRetryKind("test", errors.New("business failure"))
			}
			e.stages["stub:stub"].fetcher = fetcher
			calls := failStatusWrites(t, e, status, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := e.stageHandler(e.stages["stub:stub"])(ctx, task)
			if (err != nil) != fail || *calls != 3 || len(fetcher.calls()) != 1 || e.dedupCache.Get(task.Unique()) {
				t.Fatalf("terminal status=%d attempts=%d fetch=%d err=%v active=%v", status, *calls, len(fetcher.calls()), err, e.dedupCache.Get(task.Unique()))
			}
			want := events.TypeTaskFinished
			if fail {
				want = events.TypeTaskFailed
			}
			drainTestEvents(t, e)
			if countTaskEvents(c, want) != 1 {
				t.Fatal(c.snapshot())
			}
		})
	}
}

func TestTerminalWriteFailureNeverReportsSuccess(t *testing.T) {
	e, _, task, c := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	failStatusWrites(t, e, models.TaskStatusSuccess, -1)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	err := e.stageHandler(e.stages["stub:stub"])(ctx, task)
	if !errors.Is(err, context.DeadlineExceeded) || len(fetcher.calls()) != 1 || countTaskEvents(c, events.TypeTaskFinished) != 0 || !e.dedupCache.Get(task.Unique()) {
		t.Fatalf("unpersisted terminal state reported success: err=%v calls=%d events=%v active=%v", err, len(fetcher.calls()), c.snapshot(), e.dedupCache.Get(task.Unique()))
	}
	e.cancel = func() {}
	if drained, stats := e.Stop(time.Second); drained || stats["stub:stub"].Unfinished != 1 {
		t.Fatalf("unconfirmed terminal state hidden from shutdown: drained=%v stats=%v", drained, stats)
	}
}

func TestTerminalRetryConfirmsLostAcknowledgementWithoutDuplicateAppend(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	version := taskRunVersion{Repeat: 4, Reprocess: 2}
	f.row["repeat"], f.row["reprocess"] = int64(4), int64(2)
	f.row["status"] = int64(models.TaskStatusFailed)
	f.affected = 0 // first terminal write already committed; WHERE processing no longer matches
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.persistTerminal(ctx, task, version, models.TaskStatusFailed, errors.New("business")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.written(), "status = ? AND `repeat` = ? AND reprocess = ?") {
		t.Fatal("terminal retry can append the error again or overwrite another round", f.written())
	}
	f.row["reprocess"] = int64(3)
	f.row["status"] = int64(models.TaskStatusProcessing)
	if err := e.persistTerminal(ctx, task, version, models.TaskStatusFailed, errors.New("business")); !errors.Is(err, ErrTaskChanged) {
		t.Fatalf("stale terminal write accepted newer processing round: %v", err)
	}
}

func TestUncertainClaimNeverAssumesOwnership(t *testing.T) {
	e, f, task, c := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	failStatusWrites(t, e, models.TaskStatusProcessing, 1)
	f.affected = 0 // retry cannot tell an already-committed claim from somebody else's claim
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := e.stageHandler(e.stages["stub:stub"])(ctx, task)
	if !errors.Is(err, ErrTaskChanged) || len(fetcher.calls()) != 0 || countTaskEvents(c, events.TypeTaskFinished) != 0 {
		t.Fatalf("uncertain claim treated as owned or finished: err=%v calls=%d events=%v", err, len(fetcher.calls()), c.snapshot())
	}
	// Another queued copy that cannot claim has not resolved the first copy's
	// unknown write. It must not erase the shutdown warning for this ID.
	if err := e.stageHandler(e.stages["stub:stub"])(ctx, task); err != nil {
		t.Fatal(err)
	}
	unsettled := len(e.execution.Snapshot())
	if unsettled != 1 {
		t.Fatal("unclaimed duplicate cleared unresolved ownership")
	}
}

func TestManualRetryAndUrgencyKeepRepeatCountWhenSpilled(t *testing.T) {
	for _, action := range []string{"retry", "urgent"} {
		t.Run(action, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["repeat"] = int64(12)
			f.row["repeatable"] = int64(models.RepeatableYes)
			f.onExec = func(n int) {
				f.row["status"] = int64(models.TaskStatusPending)
				if strings.Contains(f.execs[n-1], "`repeat` + 1") {
					f.row["repeat"] = f.row["repeat"].(int64) + 1
				}
			}
			e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 1, 1))
			e.setPause(pauseScope{Site: "stub"}, pauseState{Reason: "test"})
			var err error
			if action == "retry" {
				err = e.RetryTaskVersioned(context.Background(), 7, 0, 0)
			} else {
				err = e.UrgentTask(7)
			}
			if err != nil {
				t.Fatal(err)
			}
			if e.spillBacklog() != 1 {
				t.Fatal("test did not exercise spill")
			}
			e.ForceResumeSite("stub")
			e.drainSpilled()
			if f.row["repeat"] != int64(12) {
				t.Fatalf("%s consumed polling round: %v", action, f.row["repeat"])
			}
		})
	}
}

func TestTerminalSQLGuardsRoundAndQuotesRepeat(t *testing.T) {
	db := dryRunMySQL(t)
	sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return terminalScope(tx, &Task{ID: 7}, taskRunVersion{Repeat: 4, Reprocess: 2}).Updates(taskStatusUpdates(models.TaskStatusFailed, errors.New("failure")))
	})
	for _, want := range []string{"status = ?", "`repeat` = ?", "reprocess = ?", "CONCAT(COALESCE(error, ''), ?)"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing %q in terminal SQL: %s", want, sql)
		}
	}
}

func TestClaimedRoundIsUsedByTerminalWrite(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	f.row["repeat"], f.row["reprocess"] = int64(4), int64(2)
	e.stages["stub:stub"].fetcher = &recordingFetcher{stage: "stub"}
	if err := e.stageHandler(e.stages["stub:stub"])(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	args := f.args[len(f.args)-1]
	if len(args) < 4 || args[len(args)-2] != int64(4) || args[len(args)-1] != int64(2) {
		t.Fatalf("terminal write did not use claimed database round: %v", args)
	}
}

func TestCancelledClaimReleaseFailureRemainsVisible(t *testing.T) {
	e, _, task, _ := persistenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.stages["stub:stub"].fetcher = &cancelInFetchFetcher{cancel: cancel, err: errors.New("stopping")}
	failStatusWrites(t, e, models.TaskStatusPending, -1)
	if err := e.stageHandler(e.stages["stub:stub"])(ctx, task); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	e.cancel = func() {}
	if drained, stats := e.Stop(time.Second); drained || stats["stub:stub"].Unfinished != 1 {
		t.Fatalf("failed release hidden: drained=%v stats=%v", drained, stats)
	}
}

func TestPanicTerminalWriteRetriesBeforeRethrowing(t *testing.T) {
	e, _, task, c := persistenceFixture(t)
	e.stages["stub:stub"].fetcher = &pageFetcher{panicWith: "original panic"}
	calls := failStatusWrites(t, e, models.TaskStatusFailed, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer func() {
		r := recover()
		drainTestEvents(t, e)
		if r != "original panic" || *calls != 3 || countTaskEvents(c, events.TypeTaskFailed) != 1 || e.dedupCache.Get(task.Unique()) {
			t.Fatalf("panic=%v attempts=%d events=%v", r, *calls, c.snapshot())
		}
	}()
	_ = e.stageHandler(e.stages["stub:stub"])(ctx, task)
}

func TestErrorRetryRepeatCountIndependentOfQueuePressure(t *testing.T) {
	for _, mode := range []string{"available", "full", "paused"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["repeat"] = int64(12)
			f.row["repeatable"] = int64(models.RepeatableYes)
			f.row["status"] = int64(models.TaskStatusFailed)
			f.onExec = func(n int) {
				f.row["status"] = int64(models.TaskStatusPending)
				if strings.Contains(f.execs[n-1], "`repeat` + 1") {
					f.row["repeat"] = f.row["repeat"].(int64) + 1
				}
			}
			pool := workerpool.NewWorkerPool[*Task](1, 1, 1)
			if mode == "full" {
				if err := pool.Submit(&Task{ID: 99, Stage: "stub", Site: "stub", URL: "blocker"}); err != nil {
					t.Fatal(err)
				}
			}
			e := queueEngine(t, f, pool)
			if mode == "paused" {
				e.setPause(pauseScope{Site: "stub"}, pauseState{Reason: "test"})
			}
			row := actionRow(7, "stub")
			row.Repeat, row.Repeatable = 12, models.RepeatableYes
			if !e.requeueFailedTaskIn(row) {
				t.Fatal("failed to requeue")
			}
			if mode == "paused" {
				e.ForceResumeSite("stub")
			}
			e.drainSpilled()
			if got := f.row["repeat"]; got != int64(12) {
				t.Fatalf("error retry consumed repeat budget under %s pressure: %v", mode, got)
			}
		})
	}
}

func TestExecutionRetryWriteFailureDoesNotReplayHandlerOrIncrement(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "lost-ack"}[afterCommit], func(t *testing.T) {
			e, f, task, c := persistenceFixture(t)
			fetcher := &recordingFetcher{stage: "stub", err: errors.New("retryable business failure")}
			info := e.stages["stub:stub"]
			info.fetcher = fetcher
			info.config.MaxAttempts = 3
			failure := errors.New("retry storage acknowledgement unavailable")
			writes := 0
			inject := func(tx *gorm.DB) {
				if values, ok := tx.Statement.Dest.(map[string]any); ok {
					if _, retry := values["retry"]; retry {
						writes++
						tx.AddError(failure)
					}
				}
			}
			var registerErr error
			if afterCommit {
				registerErr = e.db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:retry_lost_ack", inject)
			} else {
				registerErr = e.db.Callback().Update().Before("gorm:update").Register("test:retry_error", inject)
			}
			if registerErr != nil {
				t.Fatal(registerErr)
			}
			err := e.stageHandler(info)(context.Background(), task)
			if !errors.Is(err, failure) || writes != 1 || len(fetcher.calls()) != 1 {
				t.Fatalf("err=%v writes=%d fetches=%d", err, writes, len(fetcher.calls()))
			}
			if _, ok := e.execution.Snapshot()[task.ID]; !ok {
				t.Fatal("ambiguous retry absent from shutdown accounting")
			}
			if didWrite := strings.Contains(f.written(), "`retry`=retry +"); didWrite != afterCommit {
				t.Fatalf("write reached driver=%v lostAck=%v SQL=%s", didWrite, afterCommit, f.written())
			}

			if task.Retry != 0 || countTaskEvents(c, events.TypeTaskRetry) != 0 || countTaskEvents(c, events.TypeTaskFinished) != 0 {
				t.Fatal("unconfirmed retry was announced or executed")
			}
		})
	}
}
