package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

type stageRunVersionFetcher func(context.Context, *Task, *Engine) error

func (stageRunVersionFetcher) GetStage() string { return "stub" }
func (f stageRunVersionFetcher) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	return f(ctx, task, e)
}

func TestStageHandlerCarriesClaimedRunVersion(t *testing.T) {
	for _, cancelInFetch := range []bool{false, true} {
		name := "ordinary_fetcher_success"
		if cancelInFetch {
			name = "ordinary_fetcher_cancellation"
		}
		t.Run(name, func(t *testing.T) {
			e, f, task, eventsOut := persistenceFixture(t)
			want := taskRunVersion{Repeat: 4, Reprocess: 2}
			f.row["repeat"], f.row["reprocess"] = int64(want.Repeat), int64(want.Reprocess)
			task.Repeat = 900 // A queued copy does not define its claimed database round.
			type businessContextKey struct{}
			parent := context.WithValue(context.Background(), businessContextKey{}, "business context")
			ctx, cancel := context.WithTimeout(parent, 3*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			calls := 0
			info := e.stages["stub:stub"]
			info.fetcher = stageRunVersionFetcher(func(fetchCtx context.Context, task *Task, _ *Engine) error {
				calls++
				if task.Repeat != want.Repeat {
					t.Fatalf("fetcher received stale task repeat=%d, want %d", task.Repeat, want.Repeat)
				}
				got, ok := taskRunVersionFrom(fetchCtx)
				if !ok || got != want {
					t.Fatalf("fetcher received version=%+v present=%v, want claimed %+v", got, ok, want)
				}
				if got, ok := fetchCtx.Deadline(); !ok || !got.Equal(deadline) || fetchCtx.Value(businessContextKey{}) != "business context" {
					t.Fatal("claim context lost the caller's deadline or business value")
				}
				if cancelInFetch {
					cancel()
					if !errors.Is(fetchCtx.Err(), context.Canceled) {
						t.Fatal("claim context no longer propagates parent cancellation")
					}
					return errors.New("fetch interrupted")
				}
				return nil
			})
			err := e.stageHandler(info)(ctx, task)
			if calls != 1 || len(claimStatements(f)) != 1 {
				t.Fatalf("ordinary handler calls=%d claims=%d", calls, len(claimStatements(f)))
			}
			if cancelInFetch {
				if !errors.Is(err, context.Canceled) || len(releaseStatements(f)) != 1 || countTaskEvents(eventsOut, events.TypeTaskFinished) != 0 {
					t.Fatalf("cancelled ordinary handler err=%v releases=%d events=%v", err, len(releaseStatements(f)), eventsOut.snapshot())
				}
				assertNoTerminalStatusWrite(t, f)
			} else if err != nil || countTaskEvents(eventsOut, events.TypeTaskFinished) != 1 {
				t.Fatalf("ordinary handler success changed: err=%v events=%v", err, eventsOut.snapshot())
			}
		})
	}
}

func TestStageHandlerRetriesKeepClaimedRunVersion(t *testing.T) {
	e, f, task, eventsOut := persistenceFixture(t)
	want := taskRunVersion{Repeat: 4, Reprocess: 2}
	f.row["repeat"], f.row["reprocess"] = int64(want.Repeat), int64(want.Reprocess)
	info := e.stages["stub:stub"]
	info.config.MaxAttempts = 3
	info.config.Backoff = time.Millisecond
	calls := 0
	info.fetcher = stageRunVersionFetcher(func(ctx context.Context, _ *Task, _ *Engine) error {
		calls++
		got, ok := taskRunVersionFrom(ctx)
		if !ok || got != want {
			t.Fatalf("attempt %d changed claim version: got=%+v present=%v want=%+v", calls, got, ok, want)
		}
		if calls == 1 {
			// Simulate a newer durable round after this worker's claim. The fake
			// does not evaluate WHERE, so zero affected rows explicitly models the
			// terminal generation guard rejecting this stale worker later.
			f.mu.Lock()
			f.row["repeat"], f.row["reprocess"] = int64(9), int64(5)
			f.row["status"] = int64(models.TaskStatusProcessing)
			f.affected = 0
			f.mu.Unlock()
		}
		if calls < 3 {
			return errors.New("retry this business attempt")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := e.stageHandler(info)(ctx, task)
	if !errors.Is(err, ErrTaskChanged) || calls != 3 || len(claimStatements(f)) != 1 {
		t.Fatalf("retry must retain one claim: err=%v calls=%d claims=%d", err, calls, len(claimStatements(f)))
	}
	if countTaskEvents(eventsOut, events.TypeTaskClaimed) != 1 || countTaskEvents(eventsOut, events.TypeTaskFinished) != 0 {
		t.Fatalf("stale retried execution reported success: %v", eventsOut.snapshot())
	}
}
