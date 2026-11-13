package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func TestBatchCancellationStopsZeroPauseAndPendingJobs(t *testing.T) {
	for _, size := range []int{1, 3} {
		t.Run(string(rune('0'+size)), func(t *testing.T) {
			f := newFakeTaskDB()
			f.maxQueries = 10
			f.rows = []fakeRow{f.rowWithID(1), f.rowWithID(2), f.rowWithID(3)}
			e, _ := urgentEngine(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.ctx = ctx
			calls := 0
			n, err := e.processInBatches(e.recoverQueueQuery(), size, 1, 0, func(*models.CrawlerTask) bool {
				calls++
				cancel()
				return true
			})
			if err != nil || n != 1 || calls != 1 || len(f.queries) != 1 {
				t.Fatalf("cancelled batch: n=%d calls=%d queries=%d err=%v", n, calls, len(f.queries), err)
			}
		})
	}
}

// Cancellation after an acknowledged transition must not abandon delivery or
// convert a durable pending row into a failed task merely because pools closed.
func TestQueueCancellationAfterResetRetainsDelivery(t *testing.T) {
	for _, operation := range []string{"error", "repeat", "recover"} {
		t.Run(operation, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := queueEngine(t, f, pool)
			row := actionRow(7, "stub")
			row.Repeatable = models.RepeatableYes
			if operation == "recover" {
				row.Status = models.TaskStatusProcessing
			}
			acknowledged := false
			if err := e.db.Callback().Update().After("gorm:after_update").Register("test:stop_after_reset", func(tx *gorm.DB) {
				if tx.Error != nil || tx.RowsAffected != 1 {
					t.Errorf("reset not acknowledged: %v rows=%d", tx.Error, tx.RowsAffected)
					return
				}
				if updates, ok := tx.Statement.Dest.(map[string]any); ok {
					if status, changed := updates["status"]; changed && status != models.TaskStatusPending {
						t.Errorf("shutdown changed durable pending state: %v", status)
					}
				}
				acknowledged = true
				e.cancel()
				e.pools.Shutdown()
			}); err != nil {
				t.Fatal(err)
			}
			var ok bool
			switch operation {
			case "error":
				ok = e.requeueFailedTaskIn(row)
			case "repeat":
				ok = e.requeueRepeatTaskIn(row)
			case "recover":
				ok = e.requeueRecoverTask(row)
			}
			if !acknowledged || !ok || e.overflow.Count() != 1 || len(f.execs) == 0 {
				t.Fatalf("ack=%v delivered=%v spill=%d writes=%d SQL=%s", acknowledged, ok, e.overflow.Count(), len(f.execs), f.written())
			}
		})
	}
}

func TestQueueQueriesAndResetsCarryLifecycleContext(t *testing.T) {
	for _, operation := range []string{"query", "error", "repeat", "recover"} {
		t.Run(operation, func(t *testing.T) {
			f := newFakeTaskDB()
			e, _ := urgentEngine(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.ctx = ctx
			called := false
			check := func(tx *gorm.DB) {
				called = true
				if tx.Statement.Context != ctx {
					t.Error("SQL did not receive lifecycle context")
				}
				cancel()
			}
			var err error
			if operation == "query" {
				err = e.db.Callback().Query().Before("gorm:query").Register("test:cancel", check)
			} else {
				err = e.db.Callback().Update().Before("gorm:update").Register("test:cancel", check)
			}
			if err != nil {
				t.Fatal(err)
			}
			row := &models.CrawlerTask{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com", Status: models.TaskStatusProcessing}
			switch operation {
			case "query":
				_, err = e.processInBatches(e.recoverQueueQuery(), 1, 1, 0, func(*models.CrawlerTask) bool { t.Error("cancelled query delivered work"); return false })
			case "error":
				e.requeueFailedTaskIn(row)
			case "repeat":
				e.requeueRepeatTaskIn(row)
			case "recover":
				e.requeueRecoverTask(row)
			}
			if err != nil || !called || f.written() != "" {
				t.Fatalf("called=%v err=%v writes=%s", called, err, f.written())
			}
		})
	}
}

func TestCancelledBatchDoesNotQueryOrReset(t *testing.T) {
	f := newFakeTaskDB()
	f.maxQueries = 10
	e, _ := urgentEngine(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.ctx = ctx
	n, err := e.processInBatches(e.recoverQueueQuery(), 1, 1, 0, func(*models.CrawlerTask) bool {
		t.Error("cancelled batch admitted a task")
		return true
	})
	if err != nil || n != 0 || len(f.queries) != 0 {
		t.Fatalf("cancelled batch queried: n=%d err=%v SQL=%s", n, err, f.readSQL())
	}
	row := &models.CrawlerTask{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com"}
	if e.requeueFailedTaskIn(row) || e.requeueRepeatTaskIn(row) || e.requeueRecoverTask(row) || f.written() != "" {
		t.Fatalf("cancelled requeue wrote: %s", f.written())
	}
	if e.sleepBatchPause(0) || e.sleepBatchPause(-time.Second) {
		t.Fatal("zero pause ignored cancellation")
	}
}

func TestBatchCancellationWaitsForAdmittedCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- processConcurrently(ctx, []models.CrawlerTask{{ID: 1}, {ID: 2}}, 1, func(task *models.CrawlerTask) bool {
			if task.ID != 1 {
				t.Error("admitted work after cancellation")
			}
			close(entered)
			<-release
			return true
		})
	}()
	<-entered
	cancel()
	select {
	case <-done:
		t.Error("returned before admitted callback completed")
	default:
	}
	close(release)
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("completed count=%d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("did not drain admitted callback")
	}
}
