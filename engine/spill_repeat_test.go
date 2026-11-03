package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
)

func TestSpillRetriesCountRepeatOncePerSubmission(t *testing.T) {
	for _, failFirstWrite := range []bool{false, true} {
		name := "queue-full"
		if failFirstWrite {
			name = "write-failure-then-queue-full"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["repeat"] = int64(12)
			f.row["repeatable"] = int64(models.RepeatableYes)
			if failFirstWrite {
				f.failNext = 1
			}
			// The fake does not evaluate SQL expressions. Emulate only successful
			// repeat increments emitted by the production submission path.
			f.onExec = func(n int) {
				if f.failNext == 0 && strings.Contains(f.execs[n-1], "`repeat` + 1") {
					f.row["repeat"] = f.row["repeat"].(int64) + 1
				}
			}
			pool := workerpool.NewWorkerPool[*Task](1, 1, 1)
			if err := pool.Submit(&Task{ID: 99, Site: "stub", Stage: "stub", URL: "blocker"}); err != nil {
				t.Fatal(err)
			}
			e := queueEngine(t, f, pool)
			e.stages["stub:stub"].config.RepeatLimit = 30
			task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com/repeat", Repeatable: true}
			e.submitToPool(task, models.CrawlerTask{ID: 7, Status: models.TaskStatusPending, Repeat: 12})
			if failFirstWrite && f.row["repeat"] != int64(12) {
				t.Fatal("failed write consumed repeat count")
			}
			for i := 0; i < 40; i++ {
				e.drainSpilled()
			}
			if got := f.row["repeat"]; got != int64(13) {
				t.Fatalf("repeat after 40 full-queue drains = %v, want 13", got)
			}
			if task.Repeat != 13 {
				t.Fatalf("in-memory repeat = %d, want 13", task.Repeat)
			}
			if e.spillBacklog() != 1 || queued(t, pool) != 1 {
				t.Fatal("backpressure must retain exactly one pending task")
			}

			delivered := make(chan *Task, 3)
			pool.Start(context.Background(), func(_ context.Context, task *Task) error { delivered <- task; return nil })
			defer pool.Stop(time.Second)
			select {
			case <-delivered:
			case <-time.After(2 * time.Second):
				t.Fatal("blocker not drained")
			}
			e.drainSpilled()
			select {
			case got := <-delivered:
				if got.ID != 7 || got.Repeat != 13 {
					t.Fatalf("delivered task id=%d repeat=%d", got.ID, got.Repeat)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("pending task not delivered after capacity became available")
			}
			if e.spillBacklog() != 0 || f.row["repeat"] != int64(13) {
				t.Fatal("successful spill delivery changed the round count")
			}
			// A real new round must still increment and remain eligible for polling.
			row := actionRow(7, "stub")
			row.Repeat, row.Repeatable, row.Status = 13, models.RepeatableYes, models.TaskStatusSuccess
			if !e.requeueRepeatTaskIn(row) {
				t.Fatal("backpressure exhausted the polling limit")
			}
			select {
			case got := <-delivered:
				if got.Repeat != 14 {
					t.Fatalf("next round repeat=%d, want 14", got.Repeat)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("next round not delivered")
			}
			if f.row["repeat"] != int64(14) {
				t.Fatalf("next-round DB count=%v, want 14", f.row["repeat"])
			}
		})
	}
}
