package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

type retryPolicyCancelFetcher struct {
	recordingFetcher
	cancel context.CancelFunc
	cause  error
	panics bool
}

func (f *retryPolicyCancelFetcher) FetchHandler(context.Context, *Task, *Engine) error {
	f.cancel()
	if f.panics {
		panic(f.cause)
	}
	return f.cause
}

func TestRetryPolicyMySQLCancellationKeepsBusinessPolicy(t *testing.T) {
	for _, mode := range []string{"no-retry", "panic-no-retry", "retry-after", "cancel-only"} {
		t.Run(mode, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			e := makeEngine()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := core.WrapNoRetry(errors.New("permanent fixture failure"))
			if mode == "retry-after" {
				cause = core.WithCompensation(errors.New("deferred fixture failure"), core.CompensationPolicy{Mode: core.CompensationRetryAfter, NotBefore: time.Now().Add(time.Hour)})
			}
			if mode == "cancel-only" {
				cause = context.Canceled
			}
			info := e.stages["synthetic:detail"]
			info.config.MaxAttempts = 3
			info.fetcher = &retryPolicyCancelFetcher{cancel: cancel, cause: cause, panics: mode == "panic-no-retry"}
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/cancel-policy"}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if got := recover(); got != nil && (mode != "panic-no-retry" || got != cause) {
						t.Fatalf("unexpected panic: %v", got)
					}
				}()
				_ = e.stageHandler(info)(ctx, e.taskFromRecord(&row))
			}()
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "cancel-only" {
				if row.Status != models.TaskStatusPending || row.Error != "" {
					t.Fatalf("cancellation became failure: %+v", row)
				}
				return
			}
			if row.Status != models.TaskStatusFailed {
				t.Fatalf("business failure released for replay: %+v", row)
			}
			if mode == "retry-after" {
				if row.NextCompensationAt == nil || !row.NextCompensationAt.After(time.Now()) {
					t.Fatal("lost retry-after condition")
				}
			} else if row.RetryBlocked == nil || !*row.RetryBlocked {
				t.Fatal("lost NoRetry policy")
			}
			if result := taskstore.RetryFailed(db, row.ID, taskRunVersion{Repeat: row.Repeat, Reprocess: row.Reprocess}, row.CompensationVersion, 0); result.Error != nil || result.RowsAffected != 0 {
				t.Fatalf("automatic retry bypassed failure policy: %v/%d", result.Error, result.RowsAffected)
			}
		})
	}
}

func TestRetryPolicyMySQLStartupPreservesOnlyJournalRound(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	rows := []models.CrawlerTask{
		{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/journal", Status: models.TaskStatusProcessing, Reprocess: 2},
		{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/orphan", Status: models.TaskStatusProcessing, Reprocess: 3},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	e.breakers.smartBreakers = map[string]*smartBreaker{"synthetic": {event: &breakerEvent{Recovery: breaker.Recovery{PendingFailure: &breaker.PendingFailure{TaskID: int(rows[0].ID), Reprocess: 2}}}}}
	t.Cleanup(func() { delete(e.breakers.smartBreakers, "synthetic") })
	if err := e.preparePendingRecovery(); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if err := db.First(&rows[i], rows[i].ID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if rows[0].Status != models.TaskStatusProcessing || rows[1].Status != models.TaskStatusPending {
		t.Fatalf("incorrect journal ownership recovery: %+v", rows)
	}
	// A journal for an older generation must not suppress ordinary recovery.
	e.breakers.smartBreakers["synthetic"].event.PendingFailure.Reprocess--
	if err := e.preparePendingRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&rows[0], rows[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != models.TaskStatusPending {
		t.Fatal("stale journal blocked a later generation")
	}
}

// Uses only the opt-in isolated database created by compensationMySQL.
func TestRetryPolicyMySQLBudgetManualAndGeneration(t *testing.T) {
	db, _ := compensationMySQL(t)
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/retry", Status: models.TaskStatusProcessing, Repeatable: models.RepeatableYes}
	must := func(r *gorm.DB) {
		t.Helper()
		if r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	must(db.Create(&row))
	read := func() { t.Helper(); must(db.First(&row, row.ID)) }
	settle := func(err error) {
		t.Helper()
		must(db.Model(&row).Updates(taskStatusUpdates(models.TaskStatusFailed, err)))
		read()
	}
	retry := func() *gorm.DB {
		return taskstore.RetryFailed(db, row.ID, taskstore.RunVersion{Repeat: row.Repeat, Reprocess: row.Reprocess}, row.CompensationVersion, 2)
	}
	settle(errors.New("temporary"))
	if r := retry(); r.Error != nil || r.RowsAffected != 1 {
		t.Fatalf("automatic retry: %v/%d", r.Error, r.RowsAffected)
	}
	read()
	if row.AutoRetryCount != 1 || row.Reprocess != 1 {
		t.Fatalf("counters: %+v", row)
	}
	must(db.Model(&row).Update("status", models.TaskStatusProcessing))
	settle(core.WrapNoRetry(errors.New("permanent")))
	if r := retry(); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("blocked retry: %v/%d", r.Error, r.RowsAffected)
	}
	must(taskstore.RetryTask(db, row.ID, row.Status, taskstore.RunVersion{Repeat: row.Repeat, Reprocess: row.Reprocess}, row.CompensationVersion))
	read()
	if row.RetryBlocked == nil || !*row.RetryBlocked || row.AutoRetryCount != 1 {
		t.Fatal("manual retry reset prohibition/budget")
	}
	must(taskstore.Claim(db, row.ID))
	settle(errors.New("still fails"))
	if r := retry(); r.RowsAffected != 0 {
		t.Fatal("manual failure restored automatic retry")
	}
	must(db.Model(&row).Updates(taskStatusUpdates(models.TaskStatusSuccess, nil)))
	read()
	if row.RetryBlocked == nil || *row.RetryBlocked || row.AutoRetryCount != 0 || row.Reprocess != 2 {
		t.Fatalf("success policy: %+v", row)
	}
	// The dedicated budget can exhaust even when the generation is unrelated.
	must(db.Model(&row).Updates(map[string]any{"status": models.TaskStatusFailed, "auto_retry_count": 2}))
	if r := retry(); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("exhausted budget: %v/%d", r.Error, r.RowsAffected)
	}
	must(db.Model(&row).Updates(taskStatusUpdates(models.TaskStatusSuccess, nil)))
	read()
	// A later due change invalidates a previously selected due row.
	future := time.Now().Add(time.Hour)
	must(db.Model(&row).Update("next_repeat_at", future))
	version := taskstore.RunVersion{Repeat: row.Repeat, Reprocess: row.Reprocess}
	if r := taskstore.AdvanceRepeat(db, row.ID, version, row.CompensationVersion, 0, 60, false); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("due CAS: %v/%d", r.Error, r.RowsAffected)
	}
	// Concurrent operators selecting the same generation may advance it once.
	var wg sync.WaitGroup
	results := make(chan *gorm.DB, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- taskstore.AdvanceRepeat(db, row.ID, version, row.CompensationVersion, 0, 60, true)
		}()
	}
	wg.Wait()
	close(results)
	var count int64
	for r := range results {
		must(r)
		count += r.RowsAffected
	}
	if count != 1 {
		t.Fatalf("concurrent advance count %d", count)
	}
	must(db.Model(&row).Updates(taskStatusUpdates(models.TaskStatusSuccess, nil)))
	if r := taskstore.AdvanceRepeat(db, row.ID, version, row.CompensationVersion, 0, 60, true); r.RowsAffected != 0 {
		t.Fatal("stale generation advanced again")
	}
}

func TestRetryPolicyMySQLMigrationLegacyUnknown(t *testing.T) {
	db, _ := compensationMySQL(t)
	// Simulate the immediately preceding schema, then run the actual model migration.
	for _, column := range []string{"retry_blocked", "auto_retry_count"} {
		if err := db.Migrator().DropColumn(&models.CrawlerTask{}, column); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("INSERT INTO crawler_tasks (site,stage,url,identity_hash,status) VALUES ('synthetic','detail','https://fixture.invalid/legacy','legacy',3)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.CrawlerTask{}); err != nil {
		t.Fatal(err)
	}
	var row models.CrawlerTask
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RetryBlocked != nil || row.AutoRetryCount != 0 {
		t.Fatal("migration assigned invented policy")
	}
	if r := taskstore.RetryFailed(db, row.ID, taskstore.RunVersion{}, 0, 0); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("legacy retry: %v/%d", r.Error, r.RowsAffected)
	}
	if r := taskstore.RetryTask(db, row.ID, row.Status, taskstore.RunVersion{}, 0); r.Error != nil || r.RowsAffected != 1 {
		t.Fatalf("legacy manual retry: %v/%d", r.Error, r.RowsAffected)
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.RetryBlocked == nil || !*row.RetryBlocked {
		t.Fatal("manual legacy retry lost conservative prohibition")
	}
}

func TestRetryPolicyMySQLFirstTerminalSchedulesRepeat(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/first-repeat", Status: models.TaskStatusProcessing, Repeatable: models.RepeatableYes, RepeatInterval: 120}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	task := e.taskFromRecord(&row)
	if err := e.persistTerminal(context.Background(), task, taskRunVersion{}, models.TaskStatusSuccess, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.NextRepeatAt == nil || row.NextRepeatAt.Before(before.Add(115*time.Second)) || row.NextRepeatAt.After(time.Now().Add(125*time.Second)) {
		t.Fatalf("first terminal deadline: %v", row.NextRepeatAt)
	}
	saved := *row.NextRepeatAt
	// Lost acknowledgement retries must not move the first deadline.
	if err := e.persistTerminal(context.Background(), task, taskRunVersion{}, models.TaskStatusSuccess, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !row.NextRepeatAt.Equal(saved) {
		t.Fatal("repeated terminal write moved schedule")
	}
	// Disabling repeat between selection and mutation fences the old selection.
	if err := db.Model(&row).Update("repeatable", models.RepeatableNo).Error; err != nil {
		t.Fatal(err)
	}
	if r := taskstore.AdvanceRepeat(db, row.ID, taskRunVersion{}, row.CompensationVersion, 0, 60, true); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("repeat stop CAS: %v/%d", r.Error, r.RowsAffected)
	}
}

func TestRetryPolicyMySQLClaimInitializesRoundAndLegacyPending(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://fixture.invalid/claim", Repeatable: models.RepeatableYes}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Update("retry_blocked", nil).Error; err != nil {
		t.Fatal(err)
	}
	task := e.taskFromRecord(&row)
	claimed, version, err := e.claimForExecution(context.Background(), task)
	if err != nil || !claimed || version.Repeat != 1 || task.Repeat != 1 {
		t.Fatalf("first claim: claimed=%v version=%+v task=%d err=%v", claimed, version, task.Repeat, err)
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.RetryBlocked == nil || *row.RetryBlocked || row.Status != models.TaskStatusProcessing {
		t.Fatal("legacy pending claim lost policy/state")
	}
	if err := e.persistTerminal(context.Background(), task, version, models.TaskStatusSuccess, nil); err != nil {
		t.Fatal(err)
	}
	if r := taskstore.Claim(db, row.ID); r.Error != nil || r.RowsAffected != 0 {
		t.Fatalf("claim bypassed terminal status: %v/%d", r.Error, r.RowsAffected)
	}
}
