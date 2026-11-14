//go:build integration

package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
	"sync/atomic"
	"testing"
	"time"
)

func TestMySQLMandatoryQueuesPauseResumeAndRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			db := deliveryMySQLDB(t)
			var executions, updates atomic.Int64
			if err := db.Callback().Update().Before("gorm:update").Register("mandatory:updates", func(*gorm.DB) { updates.Add(1) }); err != nil {
				t.Fatal(err)
			}
			fetch := &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executions.Add(1); return nil }}
			cfg := &config.Config{ErrorQueue: config.ErrorQueueConfig{Interval: 50 * time.Millisecond}, RepeatQueue: config.RepeatQueueConfig{Interval: 50 * time.Millisecond}}
			e := deliveryMySQLEngineWithConfig(t, db, cfg, true, fetch)
			for i := 0; i < 2; i++ {
				task := &Task{Site: "synthetic", Stage: "sink", URL: fmt.Sprintf("https://fixture.invalid/mandatory/%d", i), Repeatable: i == 1, RepeatInterval: time.Hour}
				if err := e.SubmitEntryTask(task); err != nil {
					t.Fatal(err)
				}
				status := models.TaskStatusFailed
				if i == 1 {
					status = models.TaskStatusSuccess
				}
				if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": status, "next_repeat_at": time.Now().Add(-time.Hour)}).Error; err != nil {
					t.Fatal(err)
				}
			}
			deliveryEventually(t, func() bool { return e.errorRetriedTotal() == 1 && e.repeatRepolledTotal() == 1 })
			if executions.Load() != 0 {
				t.Fatal("paused tasks executed")
			}
			rows := deliveryRows(t, db)
			if len(rows) != 2 {
				t.Fatal("missing tasks")
			}
			for _, row := range rows {
				if row.Status != models.TaskStatusPending {
					t.Fatalf("not pending: %+v", row)
				}
			}
			if restart {
				if drained, _ := e.Stop(3 * time.Second); !drained {
					t.Fatal("stop did not drain")
				}
				before := updates.Load()
				time.Sleep(100 * time.Millisecond)
				if updates.Load() != before {
					t.Fatal("write after Stop")
				}
				e = deliveryMySQLEngine(t, db, false, fetch)
			} else {
				e.ResumeSite("synthetic")
			}
			deliveryEventually(t, func() bool {
				for _, row := range deliveryRows(t, db) {
					if row.Status != models.TaskStatusSuccess {
						return false
					}
				}
				return executions.Load() == 2
			})
			time.Sleep(150 * time.Millisecond)
			if executions.Load() != 2 {
				t.Fatalf("duplicate executions: %d", executions.Load())
			}
			if drained, _ := e.Stop(3 * time.Second); !drained {
				t.Fatal("final stop did not drain")
			}
			before := updates.Load()
			time.Sleep(100 * time.Millisecond)
			if updates.Load() != before {
				t.Fatal("write after final Stop")
			}
		})
	}
}
