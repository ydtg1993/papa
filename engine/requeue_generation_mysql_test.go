//go:build integration

package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/taskstore"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

func TestMySQLFirstRepeatScheduledAtSettlement(t *testing.T) {
	for _, period := range []time.Duration{0, time.Hour} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("period=%s/fail=%v", period, fail), func(t *testing.T) {
				db := deliveryMySQLDB(t)
				var writes, executions atomic.Int64
				db.Callback().Update().Before("gorm:update").Register("first:writes", func(*gorm.DB) { writes.Add(1) })
				e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error {
					executions.Add(1)
					if fail {
						return errors.New("synthetic")
					}
					return nil
				}})
				task := &Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/first", Repeatable: true, RepeatInterval: period}
				if err := e.SubmitEntryTask(task); err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond)
				if row := deliveryRows(t, db)[0]; row.NextRepeatAt != nil || row.Status != models.TaskStatusPending {
					t.Fatal("paused task was scheduled or executed")
				}
				before := writes.Load()
				e.ResumeSite("synthetic")
				deliveryEventually(t, func() bool {
					r := deliveryRows(t, db)[0]
					return r.Status == models.TaskStatusSuccess || r.Status == models.TaskStatusFailed
				})
				row := deliveryRows(t, db)[0]
				want := period
				if want == 0 {
					want = 12 * time.Hour
				}
				var remaining int64
				if err := db.Raw("SELECT TIMESTAMPDIFF(SECOND, NOW(), next_repeat_at) FROM crawler_tasks WHERE id = ?", row.ID).Scan(&remaining).Error; err != nil {
					t.Fatal(err)
				}
				if row.NextRepeatAt == nil || remaining < int64(want.Seconds())-2 || remaining > int64(want.Seconds()) {
					t.Fatalf("wrong first deadline: %v remaining=%d", row.NextRepeatAt, remaining)
				}
				if n, err := e.RepollRepeatableTasks(); err != nil || n != 0 {
					t.Fatalf("early repeat=%d %v", n, err)
				}
				if executions.Load() != 1 || writes.Load()-before != 2 {
					t.Fatalf("executions=%d updates=%d; want 1/2", executions.Load(), writes.Load()-before)
				}
			})
		}
	}
}

func TestMySQLFirstRepeatInterruptedThenRestart(t *testing.T) {
	db := deliveryMySQLDB(t)
	entered := make(chan struct{})
	e := deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(ctx context.Context, _ *Task, _ *Engine) error { close(entered); <-ctx.Done(); return ctx.Err() }})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/interrupted", Repeatable: true, RepeatInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("not started")
	}
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("stop failed")
	}
	if row := deliveryRows(t, db)[0]; row.Status != models.TaskStatusPending || row.NextRepeatAt != nil {
		t.Fatalf("interrupted task changed schedule: %+v", row)
	}
	var executed atomic.Int64
	e = deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executed.Add(1); return nil }})
	deliveryEventually(t, func() bool { return deliveryRows(t, db)[0].Status == models.TaskStatusSuccess })
	deadline := deliveryRows(t, db)[0].NextRepeatAt
	if deadline == nil {
		t.Fatal("missing restart deadline")
	}
	if n, err := e.RepollRepeatableTasks(); err != nil || n != 0 {
		t.Fatalf("early restart repeat=%d %v", n, err)
	}
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("final stop failed")
	}
	if executed.Load() != 1 {
		t.Fatal("duplicate restart execution")
	}
	if got := deliveryRows(t, db)[0].NextRepeatAt; got == nil || !got.Equal(*deadline) {
		t.Fatal("stop moved deadline")
	}
}

func TestMySQLGovernanceGenerationRejectsConcurrentAndStaleSnapshots(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			db := deliveryMySQLDB(t)
			var executed atomic.Int64
			fetch := &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executed.Add(1); return nil }}
			e := deliveryMySQLEngine(t, db, true, fetch)
			task := &Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/compete", Repeatable: true, RepeatInterval: time.Hour}
			if err := e.SubmitEntryTask(task); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusFailed, "next_repeat_at": gorm.Expr("DATE_SUB(NOW(), INTERVAL 1 HOUR)")}).Error; err != nil {
				t.Fatal(err)
			}
			snapshot := deliveryRows(t, db)[0]
			var wins atomic.Int64
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, repeat := range []bool{false, true} {
				wg.Add(1)
				go func(repeat bool) {
					defer wg.Done()
					<-start
					var ok bool
					if repeat {
						ok = e.requeueRepeatTaskIn(&snapshot)
					} else {
						ok = e.requeueFailedTaskIn(&snapshot)
					}
					if ok {
						wins.Add(1)
					}
				}(repeat)
			}
			close(start)
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatalf("round winners=%d", wins.Load())
			}
			row := deliveryRows(t, db)[0]
			if row.Status != models.TaskStatusPending || row.Repeat-snapshot.Repeat+row.Reprocess-snapshot.Reprocess != 1 {
				t.Fatal("wrong generation transition")
			}
			// Recreate the terminal-state ABA window without changing the winning generation.
			if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusFailed).Error; err != nil {
				t.Fatal(err)
			}
			if e.requeueFailedTaskIn(&snapshot) || e.requeueRepeatTaskWithPolicy(&snapshot, true) {
				t.Fatal("stale selection opened another generation")
			}
			// Restore the accepted pending task and exercise pause/resume or shutdown/restart.
			if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusPending, "next_repeat_at": gorm.Expr("DATE_ADD(NOW(), INTERVAL 1 HOUR)")}).Error; err != nil {
				t.Fatal(err)
			}
			if executed.Load() != 0 {
				t.Fatal("paused execution")
			}
			if restart {
				if drained, _ := e.Stop(3 * time.Second); !drained {
					t.Fatal("stop failed")
				}
				e = deliveryMySQLEngine(t, db, false, fetch)
			} else {
				e.ResumeSite("synthetic")
			}
			deliveryEventually(t, func() bool { return deliveryRows(t, db)[0].Status == models.TaskStatusSuccess })
			if n, err := e.RepollRepeatableTasks(); err != nil || n != 0 {
				t.Fatalf("repeat=%d %v", n, err)
			}
			if drained, _ := e.Stop(3 * time.Second); !drained {
				t.Fatal("final stop failed")
			}
			if executed.Load() != 1 {
				t.Fatalf("executions=%d", executed.Load())
			}
		})
	}
}

func TestMySQLRequeueRechecksDeadlineAndRetryLimit(t *testing.T) {
	db := deliveryMySQLDB(t)
	e := deliveryMySQLEngineWithConfig(t, db, &config.Config{ErrorQueue: config.ErrorQueueConfig{MaxRetry: 3}}, true, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { return nil }})
	task := &Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/deadline", Repeatable: true, RepeatInterval: time.Hour}
	if err := e.SubmitEntryTask(task); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusFailed, "reprocess": 3, "next_repeat_at": gorm.Expr("DATE_SUB(NOW(), INTERVAL 1 HOUR)")}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := deliveryRows(t, db)[0]
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("next_repeat_at", gorm.Expr("DATE_ADD(NOW(), INTERVAL 1 HOUR)")).Error; err != nil {
		t.Fatal(err)
	}
	if e.requeueRepeatTaskIn(&snapshot) {
		t.Fatal("automatic repeat ignored changed deadline")
	}
	if e.requeueFailedTaskIn(&snapshot) {
		t.Fatal("error queue ignored retry limit")
	}
	if !e.requeueRepeatTaskWithPolicy(&snapshot, true) {
		t.Fatal("explicit force must ignore deadline")
	}
	// Settlement must preserve an existing operator deadline and not schedule nonrepeatable rows.
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusProcessing).Error; err != nil {
		t.Fatal(err)
	}
	row := deliveryRows(t, db)[0]
	if err := taskstore.Settle(db, context.Background(), row.ID, models.TaskStatusSuccess, nil, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := deliveryRows(t, db)[0].NextRepeatAt; got == nil || !got.Equal(*row.NextRepeatAt) {
		t.Fatal("settlement moved existing deadline")
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusProcessing, "repeatable": 0, "next_repeat_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := taskstore.Settle(db, context.Background(), row.ID, models.TaskStatusSuccess, nil, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	if deliveryRows(t, db)[0].NextRepeatAt != nil {
		t.Fatal("nonrepeatable task scheduled")
	}
}
