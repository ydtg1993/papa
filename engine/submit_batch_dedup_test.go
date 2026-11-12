package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

func TestSubmitTasksDuplicatesKeepOnePendingDelivery(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "queue-full"
		if delayed {
			name = "delayed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			f.onExec = func(n int) {
				if strings.HasPrefix(f.execs[n-1], "INSERT INTO") {
					f.noRows = false
				}
			}
			pool := workerpool.NewWorkerPool[*Task](1, 1, 1)
			e := submitEngine(t, f, pool)
			first := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", Repeatable: true}
			duplicate := &Task{Site: first.Site, Stage: first.Stage, URL: first.URL, Repeatable: true}
			if delayed {
				first.Delay = time.Hour
			} else if err := pool.Submit(&Task{URL: "blocker"}); err != nil {
				t.Fatal(err)
			}
			if err := e.SubmitTasks([]*Task{first, duplicate}); err != nil {
				t.Fatal(err)
			}
			if delayed {
				if e.delays.Len() != 1 || queued(t, pool) != 0 || e.spillBacklog() != 0 {
					t.Fatal("duplicate must produce one delayed delivery")
				}
			} else if e.spillBacklog() != 1 || queued(t, pool) != 1 {
				t.Fatal("full queue must retain one spilled delivery")
			}
			if first.ID == 0 || duplicate.ID != first.ID {
				t.Fatal("duplicate task identity was not filled")
			}
		})
	}
}

func TestSubmitTasksDuplicateBatchKeepsNewTaskScheduled(t *testing.T) {
	for _, kind := range []string{"url", "repeatable", "idempotency-key", "same-pointer"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows, f.row["id"] = true, int64(1)
			inserts := 0
			// Emulate MySQL rejecting duplicates in a multi-row INSERT, followed
			// by a successful first insert and a duplicate second insert.
			f.onExec = func(n int) {
				q := f.execs[n-1]
				if !strings.HasPrefix(q, "INSERT INTO") {
					return
				}
				inserts++
				if strings.Contains(q, "),(") || inserts > 2 {
					f.failNext = 1
				} else {
					f.noRows = false
				}
			}
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := submitEngine(t, f, pool)
			first := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a"}
			duplicate := &Task{Site: "stub", Stage: "stub", URL: first.URL}
			if kind == "repeatable" {
				first.Repeatable, duplicate.Repeatable = true, true
			}
			if kind == "idempotency-key" {
				first.IdempotencyKey, duplicate.IdempotencyKey = "same-key", "same-key"
				duplicate.URL = "https://example.com/alias"
			}
			if kind == "same-pointer" {
				duplicate = first
			}
			if err := e.SubmitTasks([]*Task{first, duplicate}); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 || e.spillBacklog() != 0 {
				t.Fatalf("new task must be queued once: queued=%d spill=%d", queued(t, pool), e.spillBacklog())
			}
			if inserts != 1 {
				t.Fatalf("batch-local duplicates reached insert fallback: insert attempts=%d", inserts)
			}
			if first.ID == 0 || duplicate.ID != first.ID {
				t.Fatalf("duplicate IDs=%d,%d, want same nonzero ID", first.ID, duplicate.ID)
			}
			if duplicate.Repeat != first.Repeat {
				t.Fatal("duplicate lost round count")
			}
		})
	}
}

func TestSubmitTasksMixedBatchKeepsFirstOccurrence(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	first := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/a", Meta: map[string]string{"version": "first"}}
	duplicate := &Task{Site: "stub", Stage: "stub", URL: first.URL, Meta: map[string]string{"version": "second"}}
	other := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/b"}
	if err := e.SubmitTasks([]*Task{first, duplicate, other}); err != nil {
		t.Fatal(err)
	}
	if queued(t, pool) != 2 {
		t.Fatalf("[A,A,B] queued=%d, want 2", queued(t, pool))
	}
	if strings.Contains(f.writtenArgs(), "second") {
		t.Fatal("later duplicate must not override the first payload")
	}
	if duplicate.ID != first.ID || first.ID == 0 || other.ID == 0 {
		t.Fatal("task IDs were not filled")
	}
}

func TestSubmitTasksExistingDuplicateDoesNotResetStatus(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing, models.TaskStatusSuccess, models.TaskStatusFailed} {
		t.Run(statusName(status), func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(status)
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := submitEngine(t, f, pool)
			first := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/existing"}
			duplicate := &Task{Site: first.Site, Stage: first.Stage, URL: first.URL}
			if err := e.SubmitTasks([]*Task{first, duplicate}); err != nil {
				t.Fatal(err)
			}
			if f.written() != "" || queued(t, pool) != 0 {
				t.Fatal("existing non-repeat task must not be rewritten or rescheduled")
			}
			if first.ID != 7 || duplicate.ID != 7 {
				t.Fatalf("existing IDs=%d,%d, want 7", first.ID, duplicate.ID)
			}
		})
	}
}
