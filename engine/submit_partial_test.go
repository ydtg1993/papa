package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
)

func TestSubmitTasksContinuesAfterRowInsertFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		failed map[int]bool
	}{
		{"middle", 3, map[int]bool{1: true}},
		{"first", 3, map[int]bool{0: true}},
		{"last", 3, map[int]bool{2: true}},
		{"multiple", 5, map[int]bool{1: true, 3: true}},
		{"all", 3, map[int]bool{0: true, 1: true, 2: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			inserts := 0
			f.onExec = func(n int) {
				if !strings.HasPrefix(f.execs[n-1], "INSERT INTO") {
					return
				}
				inserts++
				if inserts == 1 || tc.failed[inserts-2] {
					f.failNext = 1
				}
			}
			// Only successful inserts are subsequently loaded by ID. Initial
			// dedup and failed-row conflict lookups must return no existing row.
			f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
			pool := workerpool.NewWorkerPool[*Task](1, 32, 1)
			e := submitEngine(t, f, pool)
			tasks := make([]*Task, tc.count)
			for i := range tasks {
				tasks[i] = &Task{Site: "stub", Stage: "stub", URL: fmt.Sprintf("https://example.com/%d", i)}
			}
			err := e.SubmitTasks(tasks)
			if err == nil {
				t.Fatal("failed rows must be reported")
			}
			if inserts != tc.count+1 {
				t.Fatalf("insert attempts=%d, want %d; remaining rows were skipped", inserts, tc.count+1)
			}
			if got := queued(t, pool); got != tc.count-len(tc.failed) {
				t.Fatalf("queued=%d, want %d successful rows", got, tc.count-len(tc.failed))
			}
			for i, task := range tasks {
				if tc.failed[i] {
					if task.ID != 0 || e.dedupCache.Get(task.Unique()) {
						t.Fatalf("failed row %d was treated as persisted", i)
					}
					if !strings.Contains(err.Error(), task.URL) {
						t.Fatalf("missing failed row %d in aggregate error: %v", i, err)
					}
				} else if task.ID == 0 || !e.dedupCache.Get(task.Unique()) {
					t.Fatalf("successful row %d lost its identity or ownership", i)
				}
			}
			// Retrying successful rows must not enqueue them a second time.
			var successful []*Task
			for i, task := range tasks {
				if !tc.failed[i] {
					successful = append(successful, task)
				}
			}
			if err := e.SubmitTasks(successful); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != len(successful) {
				t.Fatal("successful retry duplicated delivery")
			}
			var rejected []*Task
			for i, task := range tasks {
				if tc.failed[i] {
					rejected = append(rejected, task)
				}
			}
			if err := e.SubmitTasks(rejected); err != nil {
				t.Fatalf("failed rows could not be retried after recovery: %v", err)
			}
			if queued(t, pool) != tc.count {
				t.Fatal("failed rows were cached or lost instead of retried")
			}
		})
	}
}

func TestSubmitTasksPartialSuccessHonoursDelay(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	inserts := 0
	f.onExec = func(n int) {
		if strings.HasPrefix(f.execs[n-1], "INSERT INTO") {
			inserts++
			if inserts == 1 || inserts == 3 {
				f.failNext = 1
			}
		}
	}
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	tasks := []*Task{
		{Site: "stub", Stage: "stub", URL: "https://example.com/A", Delay: time.Hour},
		{Site: "stub", Stage: "stub", URL: "https://example.com/B"},
		{Site: "stub", Stage: "stub", URL: "https://example.com/C"},
	}
	if err := e.SubmitTasks(tasks); err == nil {
		t.Fatal("B failure was hidden")
	}
	if e.delayHeap.Len() != 1 || queued(t, pool) != 1 {
		t.Fatalf("A must be delayed and C queued: delayed=%d queued=%d", e.delayHeap.Len(), queued(t, pool))
	}
}

func TestSubmitTasksRetainsInsertedTaskOnReloadFailure(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.onExec = func(n int) {
		if strings.HasPrefix(f.execs[n-1], "INSERT INTO") {
			f.noRows = false
		}
	}
	failedRead := false
	f.onQuery = func(n int) {
		if !failedRead && strings.Contains(f.queries[n-1], "WHERE id = ?") {
			f.failQueries = 1
			failedRead = true
		}
	}
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/A"}
	if err := e.SubmitTasks([]*Task{task}); err == nil {
		t.Fatal("reload failure must be reported")
	}
	if task.ID == 0 || e.spillBacklog() != 1 || queued(t, pool) != 0 {
		t.Fatal("persisted task must remain in spill until its state can be read")
	}
	e.drainSpilled()
	if e.spillBacklog() != 0 || queued(t, pool) != 1 {
		t.Fatal("persisted task was not retried after DB recovered")
	}
}

func TestSubmitTasksReloadFailureKeepsNotBefore(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.onExec = func(n int) {
		if strings.HasPrefix(f.execs[n-1], "INSERT INTO") {
			f.noRows = false
		}
	}
	f.onQuery = func(n int) {
		if strings.Contains(f.queries[n-1], "WHERE id = ?") {
			f.failQueries = 1
		}
	}
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com/delayed", NotBefore: time.Now().Add(time.Hour)}
	if err := e.SubmitTasks([]*Task{task}); err == nil {
		t.Fatal("reload failure must be reported")
	}
	if e.delayHeap.Len() != 1 || e.spillBacklog() != 0 || queued(t, pool) != 0 {
		t.Fatal("reload failure must not route a future task into immediate spill delivery")
	}
}
