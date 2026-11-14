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
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

type noAutoRetryError struct{}

func (noAutoRetryError) Error() string   { return "custom permanent" }
func (noAutoRetryError) Retryable() bool { return false }
func retryRow(t *testing.T, db *gorm.DB) models.CrawlerTask {
	t.Helper()
	rows := deliveryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	return rows[0]
}
func assertNoAutomaticRetry(t *testing.T, e *Engine) {
	t.Helper()
	for _, fn := range []func() (int, error){e.ProcessErrorQueue, e.RepollRepeatableTasks, e.ForceRepollRepeatableTasks} {
		n, err := fn()
		if err != nil || n != 0 {
			t.Fatalf("blocked automatic work: %d %v", n, err)
		}
	}
}

func TestMySQLRetryPolicyBudgetPauseRestartAndManual(t *testing.T) {
	db := deliveryMySQLDB(t)
	var succeed atomic.Bool
	var attempts atomic.Int64
	fetch := &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error {
		attempts.Add(1)
		if succeed.Load() {
			return nil
		}
		return errors.New("transient")
	}}
	cfg := &config.Config{ErrorQueue: config.ErrorQueueConfig{MaxRetry: 2}}
	e := deliveryMySQLEngineWithConfig(t, db, cfg, true, fetch)
	task := &Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/budget", Repeatable: true, RepeatInterval: time.Hour}
	if err := e.SubmitEntryTask(task); err != nil {
		t.Fatal(err)
	}
	run := func() {
		e.ResumeSite("synthetic")
		deliveryEventually(t, func() bool {
			r := retryRow(t, db)
			return r.Status == models.TaskStatusFailed || r.Status == models.TaskStatusSuccess
		})
		e.PauseSite("synthetic", "test")
	}
	run()
	if retryRow(t, db).AutoRetryCount != 0 {
		t.Fatal("initial attempt consumed retry budget")
	}
	for i := 1; i <= 2; i++ {
		n, err := e.ProcessErrorQueue()
		if err != nil || n != 1 {
			t.Fatalf("retry %d: %d %v", i, n, err)
		}
		row := retryRow(t, db)
		if row.AutoRetryCount != i || row.Reprocess != i || row.Status != models.TaskStatusPending {
			t.Fatalf("bad pending count: %+v", row)
		}
		if i == 1 {
			if drained, _ := e.Stop(3 * time.Second); !drained {
				t.Fatal("stop failed")
			}
			e = deliveryMySQLEngineWithConfig(t, db, cfg, true, fetch)
			if retryRow(t, db).AutoRetryCount != 1 {
				t.Fatal("restart changed budget")
			}
		}
		run()
	}
	assertNoAutomaticRetry(t, e)
	row := retryRow(t, db)
	if err := e.RetryTask(row.ID, row.Reprocess); err != nil {
		t.Fatal(err)
	}
	run()
	if retryRow(t, db).AutoRetryCount != 2 {
		t.Fatal("manual retry reset/consumed budget")
	}
	assertNoAutomaticRetry(t, e)
	succeed.Store(true)
	row = retryRow(t, db)
	if err := e.RetryTask(row.ID, row.Reprocess); err != nil {
		t.Fatal(err)
	}
	run()
	row = retryRow(t, db)
	if row.AutoRetryCount != 0 || row.RetryBlocked == nil || *row.RetryBlocked || row.Reprocess != 4 {
		t.Fatalf("success reset generation or retained budget: %+v", row)
	}
	succeed.Store(false)
	n, err := e.ForceRepollRepeatableTasks()
	if n != 1 || err != nil {
		t.Fatalf("success repeat: %d %v", n, err)
	}
	run()
	if retryRow(t, db).AutoRetryCount != 0 {
		t.Fatal("periodic execution consumed error budget")
	}
	n, err = e.ProcessErrorQueue()
	if n != 1 || err != nil {
		t.Fatalf("new failure episode: %d %v", n, err)
	}
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("final stop failed")
	}
	if attempts.Load() != 6 {
		t.Fatalf("unexpected duplicate executions: %d", attempts.Load())
	}
}

func TestMySQLNoRetryPersistsAndManualSuccessClears(t *testing.T) {
	for _, kind := range []string{"NoRetry", "NoRetryKind", "custom"} {
		t.Run(kind, func(t *testing.T) {
			db := deliveryMySQLDB(t)
			var mode atomic.Int32
			var attempts atomic.Int64
			fetch := &deliveryMySQLFetcher{stage: "sink", attempts: 3, fn: func(context.Context, *Task, *Engine) error {
				attempts.Add(1)
				switch mode.Load() {
				case 0:
					if kind == "custom" {
						return fmt.Errorf("wrapped: %w", noAutoRetryError{})
					}
					if kind == "NoRetryKind" {
						return core.WrapNoRetryKind("fixture", errors.New("permanent"))
					}
					return core.WrapNoRetry(errors.New("permanent"))
				case 1:
					return errors.New("transient after manual retry")
				default:
					return nil
				}
			}}
			e := deliveryMySQLEngine(t, db, true, fetch)
			if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/no-retry", Repeatable: true, RepeatInterval: time.Hour}); err != nil {
				t.Fatal(err)
			}
			e.ResumeSite("synthetic")
			deliveryEventually(t, func() bool { return retryRow(t, db).Status == models.TaskStatusFailed })
			e.PauseSite("synthetic", "test")
			row := retryRow(t, db)
			if row.RetryBlocked == nil || !*row.RetryBlocked || attempts.Load() != 1 {
				t.Fatal("NoRetry did not stop immediate/automatic retry")
			}
			assertNoAutomaticRetry(t, e)
			if drained, _ := e.Stop(3 * time.Second); !drained {
				t.Fatal("stop failed")
			}
			e = deliveryMySQLEngine(t, db, true, fetch)
			assertNoAutomaticRetry(t, e)
			mode.Store(1)
			if err := e.RetryTask(row.ID, row.Reprocess); err != nil {
				t.Fatal(err)
			}
			e.ResumeSite("synthetic")
			deliveryEventually(t, func() bool { return retryRow(t, db).Status == models.TaskStatusFailed })
			e.PauseSite("synthetic", "test")
			if r := retryRow(t, db); r.RetryBlocked == nil || !*r.RetryBlocked {
				t.Fatal("ordinary manual failure cleared prohibition")
			}
			assertNoAutomaticRetry(t, e)
			mode.Store(2)
			row = retryRow(t, db)
			if err := e.RetryTask(row.ID, row.Reprocess); err != nil {
				t.Fatal(err)
			}
			e.ResumeSite("synthetic")
			deliveryEventually(t, func() bool { return retryRow(t, db).Status == models.TaskStatusSuccess })
			if r := retryRow(t, db); r.RetryBlocked == nil || *r.RetryBlocked || r.AutoRetryCount != 0 {
				t.Fatal("manual success did not reset restriction")
			}
		})
	}
}

func TestMySQLRetryPolicyConcurrentManualAndAutomatic(t *testing.T) {
	db := deliveryMySQLDB(t)
	e := deliveryMySQLEngineWithConfig(t, db, &config.Config{ErrorQueue: config.ErrorQueueConfig{MaxRetry: 1}}, true, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { return nil }})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/race", Repeatable: true}); err != nil {
		t.Fatal(err)
	}
	row := retryRow(t, db)
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", row.ID).Update("status", models.TaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	row = retryRow(t, db)
	var wins atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if e.requeueFailedTaskIn(&row) {
					wins.Add(1)
				}
			} else if e.RetryTask(row.ID, row.Reprocess) == nil {
				wins.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	r := retryRow(t, db)
	if wins.Load() != 1 || r.Reprocess != row.Reprocess+1 || r.AutoRetryCount > 1 {
		t.Fatalf("double retry: wins=%d row=%+v", wins.Load(), r)
	}
	if e.requeueRepeatTaskWithPolicy(&row, true) {
		t.Fatal("force repeat accepted failed snapshot")
	}
}

func TestMySQLRetryPolicyMigrationIsConservativeAndIdempotent(t *testing.T) {
	db := deliveryMySQLDB(t)
	// Only the generated disposable database from deliveryMySQLDB is modified.
	if err := db.Exec("ALTER TABLE crawler_tasks DROP INDEX idx_task_auto_retry, DROP COLUMN auto_retry_count, DROP COLUMN retry_blocked").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO crawler_tasks (site,stage,url,url_hash,status,repeatable,reprocess) VALUES ('synthetic','sink','https://fixture.invalid/legacy','legacy',3,1,9)").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := database.Migrate(db, &config.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	row := retryRow(t, db)
	var columns []string
	if err := db.Raw("SELECT COLUMN_NAME FROM information_schema.statistics WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'crawler_tasks' AND INDEX_NAME = 'idx_task_auto_retry' ORDER BY SEQ_IN_INDEX").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if len(columns) != 3 || columns[0] != "status" || columns[1] != "retry_blocked" || columns[2] != "auto_retry_count" {
		t.Fatalf("retry index columns: %v", columns)
	}
	if row.RetryBlocked != nil || row.AutoRetryCount != 0 || row.Reprocess != 9 {
		t.Fatal("migration inferred or overwrote historical policy")
	}
	e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { return errors.New("legacy failure") }})
	assertNoAutomaticRetry(t, e)
	if err := e.RetryTask(row.ID, row.Reprocess); err != nil {
		t.Fatal(err)
	}
	r := retryRow(t, db)
	if r.RetryBlocked == nil || !*r.RetryBlocked {
		t.Fatal("manual legacy replay did not retain prohibition")
	}
	e.ResumeSite("synthetic")
	deliveryEventually(t, func() bool { return retryRow(t, db).Status == models.TaskStatusFailed })
	assertNoAutomaticRetry(t, e)
}

func TestMySQLAutomaticRetryShutdownDoesNotChargeAgain(t *testing.T) {
	db := deliveryMySQLDB(t)
	entered := make(chan struct{})
	cfg := &config.Config{ErrorQueue: config.ErrorQueueConfig{MaxRetry: 1}}
	e := deliveryMySQLEngineWithConfig(t, db, cfg, true, &deliveryMySQLFetcher{stage: "sink", fn: func(ctx context.Context, _ *Task, _ *Engine) error { close(entered); <-ctx.Done(); return ctx.Err() }})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/stop-retry"}); err != nil {
		t.Fatal(err)
	}
	row := retryRow(t, db)
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", row.ID).Update("status", models.TaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := e.ProcessErrorQueue(); n != 1 || err != nil {
		t.Fatalf("retry %d %v", n, err)
	}
	e.ResumeSite("synthetic")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("not executing")
	}
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("stop failed")
	}
	row = retryRow(t, db)
	if row.Status != models.TaskStatusPending || row.AutoRetryCount != 1 || row.RetryBlocked == nil || *row.RetryBlocked {
		t.Fatal("cancellation changed failure policy")
	}
	e = deliveryMySQLEngineWithConfig(t, db, cfg, false, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { return errors.New("transient") }})
	deliveryEventually(t, func() bool { return retryRow(t, db).Status == models.TaskStatusFailed })
	if retryRow(t, db).AutoRetryCount != 1 {
		t.Fatal("restart charged twice")
	}
	assertNoAutomaticRetry(t, e)
}
