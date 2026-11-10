package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/pkg/account"
)

func TestAccountPoolTaskRoundTripAndDedupIdentity(t *testing.T) {
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", AccountPool: "members"}
	other := *task
	other.AccountPool = "other"
	if task.Unique() != other.Unique() {
		t.Fatal("account pool changed task uniqueness")
	}
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	record := task.toModel()
	record.ID = 7
	got := e.taskFromRecord(&record)
	if got.AccountPool != "members" {
		t.Fatal("recovery lost account pool")
	}
}

func TestAccountPoolDedupConflictIncludesCacheAndFallback(t *testing.T) {
	for _, path := range []string{"cache", "database", "insert-fallback"} {
		t.Run(path, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["account_pool"] = "old"
			e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
			task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", AccountPool: "new"}
			if path == "cache" {
				e.dedupCache.Add(task.Unique())
			}
			if path == "insert-fallback" {
				f.noRows = true
				f.failNext = 1
				f.onQuery = func(n int) {
					if n > 1 {
						f.noRows = false
					}
				}
			}
			err := e.SubmitTask(task)
			if !errors.Is(err, ErrTaskOwnershipConflict) {
				t.Fatalf("want ownership conflict, got %v", err)
			}
			if strings.Contains(f.written(), "UPDATE") {
				t.Fatal("conflict overwrote existing task")
			}
		})
	}
}

func TestAccountPoolBatchConflictingMembersNeverInsert(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	a := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", AccountPool: "one"}
	b := *a
	b.AccountPool = "two"
	if err := e.SubmitTasks([]*Task{a, &b}); !errors.Is(err, ErrTaskOwnershipConflict) {
		t.Fatalf("want conflict: %v", err)
	}
	if f.written() != "" {
		t.Fatal("conflicting batch wrote records")
	}
}

func TestAccountPoolExistingTaskPreservesStoredOwnership(t *testing.T) {
	f := newFakeTaskDB()
	f.row["account_pool"] = "original"
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if task.ID != 7 || task.AccountPool != "original" {
		t.Fatalf("dedup did not preserve original identity: %+v", task)
	}
}

func TestAccountPoolUnknownCannotInsertAndFlowCannotBypassGuard(t *testing.T) {
	for _, batch := range []bool{false, true} {
		f := newFakeTaskDB()
		f.noRows = true
		e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
		task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/new", AccountPool: "unknown"}
		var err error
		if batch {
			err = e.SubmitTasks([]*Task{task})
		} else {
			err = e.SubmitTask(task)
		}
		if !errors.Is(err, account.ErrPoolUnknown) || f.written() != "" {
			t.Fatalf("unregistered pool inserted task: batch=%v err=%v", batch, err)
		}
	}
	if err := (&Engine{}).SubmitFlowTask(context.Background(), "flow", 0, &Task{AccountPool: "p"}); !errors.Is(err, ErrFlowInvalid) {
		t.Fatal("flow bypassed pool reference protection", err)
	}
}

func TestAccountPoolCachedDuplicateDoesNotEnqueue(t *testing.T) {
	f := newFakeTaskDB()
	f.row["account_pool"] = "p"
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", AccountPool: "p"}
	e.dedupCache.Add(task.Unique())
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if task.ID != 7 || queued(t, pool) != 0 || f.written() != "" {
		t.Fatal("cached duplicate was re-enqueued or rewritten")
	}
}
