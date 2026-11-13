package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PAPA_COMPENSATION_TEST_DSN explicitly opts into creating and dropping a fresh
// random test database. The supplied DSN's database is never used or migrated.
func compensationMySQL(t *testing.T) (*gorm.DB, func() *Engine) {
	return compensationMySQLWithLocation(t, nil)
}

func compensationMySQLWithLocation(t *testing.T, location *time.Location) (*gorm.DB, func() *Engine) {
	return compensationMySQLWithOptions(t, location, nil)
}

func compensationMySQLWithOptions(t *testing.T, location *time.Location, foundRows *bool) (*gorm.DB, func() *Engine) {
	t.Helper()
	dsn := os.Getenv("PAPA_COMPENSATION_TEST_DSN")
	if dsn == "" {
		path := os.Getenv("PAPA_RUNTIME_TEST_CONFIG")
		if path == "" {
			t.Skip("PAPA_COMPENSATION_TEST_DSN/PAPA_RUNTIME_TEST_CONFIG not set")
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal("cannot read test connection configuration")
		}
		dsn = cfg.DB.DSN
	}
	c, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test database connection")
	}
	c.DBName = ""
	c.ParseTime = true
	if foundRows != nil {
		c.ClientFoundRows = *foundRows
	}
	if location != nil {
		c.Loc = location
	}
	c.Timeout = 5 * time.Second
	server, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test server")
	}
	t.Cleanup(func() { _ = server.Close() })
	name := "papa_compensation_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	cleanup, err := testdb.Create(context.Background(), server, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	c.DBName = name
	db, err := gorm.Open(mysql.Open(c.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open isolated compensation test database")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&models.CrawlerTask{}); err != nil {
		t.Fatal(err)
	}
	makeEngine := func() *Engine {
		cfg := &config.Config{}
		cfg.Crawler.TraceFiles.Dir = t.TempDir()
		cfg.RecoverQueue.Enabled = true
		cfg.ErrorQueue.MaxRetry = 4
		cfg.ErrorQueue.BatchSize = 10
		cfg.ErrorQueue.WorkerCount = 1
		cfg.RepeatQueue.Interval = time.Hour
		e := NewEngine(db, cfg, testLoggerSet())
		t.Cleanup(func() { e.Stop(time.Second) })
		e.stages["synthetic:detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: "synthetic"}}
		e.pools.Set("synthetic:detail", workerpool.NewWorkerPool[*Task](1, 100, 1))
		return e
	}
	return db, makeEngine
}

func TestCompensationMySQLPersistenceRecoveryCASAndIdentity(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	ctx := context.Background()
	create := func(path, mode string, at *time.Time) models.CrawlerTask {
		t.Helper()
		row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/" + path, Status: models.TaskStatusFailed, Repeatable: models.RepeatableYes, CompensationMode: mode, NextCompensationAt: at, CompensationVersion: 1, Reprocess: 0}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	future := time.Now().Add(time.Hour)
	manual := create("manual", "manual", nil)
	permanent := create("permanent", "permanent", nil)
	later := create("later", "retry_after", &future)
	for _, row := range []models.CrawlerTask{manual, permanent, later} {
		if e.requeueFailedTaskIn(&row) || e.requeueRepeatTaskIn(&row) {
			t.Fatal("blocked task requeued")
		}
		if err := e.RetryTaskVersioned(context.Background(), row.ID, 0, 0); !errors.Is(err, ErrCompensationBlocked) {
			t.Fatal("manual bypass", err)
		}
		if err := db.Model(&models.CrawlerTask{}).Where("id = ?", row.ID).Update("status", models.TaskStatusPending).Error; err != nil {
			t.Fatal(err)
		}
		if claimed, err := e.claimTask(e.taskFromRecord(&row)); err != nil || claimed {
			t.Fatal("claim bypass", claimed, err)
		}
		if err := db.Model(&models.CrawlerTask{}).Where("id = ?", row.ID).Update("status", models.TaskStatusFailed).Error; err != nil {
			t.Fatal(err)
		}
	}
	e = makeEngine() // persisted conditions survive engine replacement
	preview, err := e.PreviewCompensation(ctx, CompensationFilter{Site: "synthetic"})
	if err != nil || preview.Blocked != 3 || preview.Eligible != 0 {
		t.Fatal("restart lost conditions", preview, err)
	}
	if err := e.ReleaseTaskCompensation(ctx, permanent.ID, 1); !errors.Is(err, ErrTaskChanged) {
		t.Fatal("permanent failure revived", err)
	}
	if err := e.ReleaseTaskCompensation(ctx, manual.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.ReleaseTaskCompensation(ctx, manual.ID, 1); !errors.Is(err, ErrTaskChanged) {
		t.Fatal("stale release accepted", err)
	}
	results, err := e.BatchCompensate(ctx, []CompensationRequest{{ID: manual.ID, Version: 1}})
	if err != nil || results[0].Submitted {
		t.Fatal("stale preview accepted", results, err)
	}
	row, err := e.loadTask(context.Background(), manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.requeueFailedTaskIn(row) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("concurrent compensation winners=%d", won.Load())
	}
	stored, err := e.loadTask(context.Background(), manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reprocess != 1 || stored.ID != manual.ID || stored.IdentityHash != manual.IdentityHash {
		t.Fatalf("lost original identity or spent duplicate generation: %+v", stored)
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", later.ID).Update("next_compensation_at", gorm.Expr("NOW() - INTERVAL 1 SECOND")).Error; err != nil {
		t.Fatal(err)
	}
	row, err = e.loadTask(context.Background(), later.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !e.requeueFailedTaskIn(row) {
		t.Fatal("elapsed deadline failed to requeue")
	}
	// Persist associations without trace, then filter by older retained identity.
	task := e.taskFromRecord(stored)
	if ok, err := e.claimTask(task); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	for _, id := range []string{"old-account", "new-account"} {
		bound := account.WithAttempt(ctx, &account.Attempt{Account: account.Key{Site: "synthetic", ID: id}, SelectionVersion: 2, CredentialVersion: 3})
		if err := e.persistAccountAttempt(bound, task, taskRunVersion{Reprocess: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.persistTerminal(ctx, task, taskRunVersion{Reprocess: 1}, models.TaskStatusFailed, core.ManualCompensation(errors.New("synthetic"), "review")); err != nil {
		t.Fatal(err)
	}
	preview, err = e.PreviewCompensation(ctx, CompensationFilter{Site: "synthetic", AccountID: "old-account"})
	if err != nil || len(preview.Items) != 1 || len(preview.Items[0].AccountAttempts) != 2 {
		t.Fatal("JSON association query", preview, err)
	}
}
