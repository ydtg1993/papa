package engine

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Opt-in integration tests only migrate and write the dedicated test schema.
func repeatMySQL(t *testing.T, foundRows bool) (*gorm.DB, string, func() *Engine) {
	t.Helper()
	dsn := os.Getenv("PAPA_REPEAT_TEST_DSN")
	var db *gorm.DB
	if dsn == "" {
		db, _ = compensationMySQLWithOptions(t, nil, &foundRows)
	} else {
		var err error
		c, err := driver.ParseDSN(dsn)
		if err != nil || c.DBName != "papa_repeat_test" {
			t.Fatal("requires dedicated papa_repeat_test database")
		}
		c.ParseTime, c.ClientFoundRows = true, foundRows
		db, err = gorm.Open(mysql.Open(c.FormatDSN()), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal("cannot open dedicated database")
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sqlDB.Close() })
		if err := db.AutoMigrate(&models.CrawlerTask{}); err != nil {
			t.Fatal(err)
		}

	}
	site := "repeat-" + uuid.NewString()
	t.Cleanup(func() {
		if err := db.Where("site = ?", site).Delete(&models.CrawlerTask{}).Error; err != nil {
			t.Error(err)
		}
	})
	makeEngine := func() *Engine {
		cfg := &config.Config{}
		cfg.RecoverQueue.Enabled = true
		cfg.RepeatQueue.BatchSize, cfg.RepeatQueue.WorkerCount, cfg.RepeatQueue.Interval = 16, 2, time.Hour
		cfg.RecoverQueue.BatchSize, cfg.RecoverQueue.WorkerCount = 16, 2
		e := NewEngine(db, cfg, testLoggerSet())
		t.Cleanup(func() { e.Stop(time.Second) })
		if err := e.AddStage("detail", StageConfig{Site: site, WorkerCount: 1, QueueSize: 8, RepeatLimit: 13}, nil, nil); err != nil {
			t.Fatal(err)
		}
		// Pause only this synthetic stage. Public submissions still perform real SQL.
		e.setPause(pauseScope{Site: site, Stage: "detail"}, pauseState{Source: PauseSourceManual})
		return e
	}
	return db, site, makeEngine
}

func repeatMySQLStored(t *testing.T, db *gorm.DB, id int, want int) models.CrawlerTask {
	t.Helper()
	var row models.CrawlerTask
	if err := db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Repeat != want {
		t.Fatalf("stored repeat=%d, want %d for the same pending round", row.Repeat, want)
	}
	return row
}

func TestRepeatSubmissionMySQLConcurrent(t *testing.T) {
	for _, foundRows := range []bool{false, true} {
		for _, entry := range []string{"SubmitTask", "SubmitTasks", "mixed"} {
			name := entry + "/changed-rows"
			if foundRows {
				name = entry + "/found-rows"
			}
			t.Run(name, func(t *testing.T) {
				db, site, makeEngine := repeatMySQL(t, foundRows)
				e := makeEngine()
				base := &Task{Site: site, Stage: "detail", URL: "https://example.invalid/pending", Repeatable: true}
				if err := e.SubmitTask(base); err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&models.CrawlerTask{}).Where("id = ?", base.ID).Update("repeat", 12).Error; err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						task := &Task{Site: site, Stage: "detail", URL: base.URL, Repeatable: true}
						var err error
						if entry == "SubmitTasks" || entry == "mixed" && i%2 == 0 {
							err = e.SubmitTasks([]*Task{task})
						} else {
							err = e.SubmitTask(task)
						}
						if err != nil {
							t.Error(err)
						}
					}(i)
				}
				close(start)
				wg.Wait()
				repeatMySQLStored(t, db, base.ID, 12)
				if e.spillBacklog() != 2 {
					t.Fatalf("spill=%d, want 2 distinct rounds (old round remains fenced until drain)", e.spillBacklog())
				}
			})
		}
	}
}

func TestRepeatSubmissionMySQLRoundsAndRestart(t *testing.T) {
	db, site, makeEngine := repeatMySQL(t, false)
	e := makeEngine()
	task := &Task{Site: site, Stage: "detail", URL: "https://example.invalid/round", Repeatable: true}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	repeatMySQLStored(t, db, task.ID, 1)
	for i := 0; i < 4; i++ {
		e.drainSpilled()
	}
	repeatMySQLStored(t, db, task.ID, 1)
	// Recovery uses durable state only, including a previously claimed round.
	for _, status := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing} {
		e.Stop(time.Second)
		if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": status, "updated_at": time.Now().Add(-time.Minute)}).Error; err != nil {
			t.Fatal(err)
		}
		e = makeEngine()
		if n, err := e.ProcessRecoverQueue(); err != nil || n != 1 {
			t.Fatalf("recover=%d err=%v", n, err)
		}
		repeatMySQLStored(t, db, task.ID, 1)
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusSuccess, "repeat": 12}).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.ForceRepollRepeatableTasks(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	row := repeatMySQLStored(t, db, task.ID, 13)
	if row.Status != models.TaskStatusPending || row.LastRepeatAt == nil || row.NextRepeatAt == nil {
		t.Fatal("round reset did not atomically persist status and schedule")
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := e.ForceRepollRepeatableTasks(); err != nil || n != 0 {
		t.Fatalf("capped poll=%d err=%v", n, err)
	}
	repeatMySQLStored(t, db, task.ID, 13)
}

func TestRepeatSubmissionMySQLFirstRoundConcurrent(t *testing.T) {
	db, site, makeEngine := repeatMySQL(t, false)
	e := makeEngine()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			task := &Task{Site: site, Stage: "detail", URL: "https://example.invalid/first", Repeatable: true}
			var err error
			if i%2 == 0 {
				err = e.SubmitTask(task)
			} else {
				err = e.SubmitTasks([]*Task{task})
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	var rows []models.CrawlerTask
	if err := db.Where("site = ?", site).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("created %d rows, want 1", len(rows))
	}
	repeatMySQLStored(t, db, int(rows[0].ID), 1)
	// Sequential resubmissions with newly allocated objects must also be idempotent.
	for i := 0; i < 4; i++ {
		task := &Task{Site: site, Stage: "detail", URL: rows[0].URL, Repeatable: true}
		if err := e.SubmitTask(task); err != nil {
			t.Fatal(err)
		}
		if task.Repeat != 1 {
			t.Fatalf("resubmitted task repeat=%d, want 1", task.Repeat)
		}
	}
	repeatMySQLStored(t, db, int(rows[0].ID), 1)
}

func TestRepeatSubmissionMySQLLostAcknowledgement(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "first-round"
		if reset {
			name = "next-round-reset"
		}
		t.Run(name, func(t *testing.T) {
			db, site, makeEngine := repeatMySQL(t, false)
			e := makeEngine()
			task := &Task{Site: site, Stage: "detail", URL: "https://example.invalid/lost-ack", Repeatable: true}
			want := 1
			if reset {
				if err := e.SubmitTask(task); err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": models.TaskStatusSuccess, "repeat": 12}).Error; err != nil {
					t.Fatal(err)
				}
				want = 13
			}
			// Report a failure AFTER commit: the database changed but the caller
			// cannot know it. Recovery must not consume another polling round.
			var lost atomic.Bool
			if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:repeat_lost_ack", func(tx *gorm.DB) {
				if tx.Error == nil && tx.RowsAffected > 0 && lost.CompareAndSwap(false, true) {
					tx.AddError(errors.New("synthetic acknowledgement lost after commit"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Update().Remove("test:repeat_lost_ack") })
			if reset {
				if _, err := e.ForceRepollRepeatableTasks(); err != nil {
					t.Fatal(err)
				}
			} else if err := e.SubmitTask(task); err != nil {
				t.Fatal(err)
			}
			if !lost.Load() {
				t.Fatal("fault was not injected")
			}
			row := repeatMySQLStored(t, db, task.ID, want)
			if row.Status != models.TaskStatusPending {
				t.Fatal("committed pending state lost")
			}
			e.Stop(time.Second)
			if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("updated_at", time.Now().Add(-time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			e = makeEngine()
			if n, err := e.ProcessRecoverQueue(); err != nil || n != 1 {
				t.Fatalf("recover=%d err=%v", n, err)
			}
			repeatMySQLStored(t, db, task.ID, want)
		})
	}
}
