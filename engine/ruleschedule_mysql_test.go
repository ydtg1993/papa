package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func scheduleMySQL(t *testing.T) (*gorm.DB, *RuleSchedule, *testRuleCatalog, *Task) {
	t.Helper()
	dsn := os.Getenv("PAPA_RULE_SCHEDULE_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_RULE_SCHEDULE_TEST_DSN not set; real schedule MySQL verification not run")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_rule_test_schedule" {
		t.Fatal("requires dedicated papa_rule_test_schedule database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&models.CrawlerTask{}, &models.RuleSchedule{}); err != nil {
		t.Fatal(err)
	}
	s, catalog := scheduleTest(t)
	site := "schedule-" + uuid.NewString()
	s.config.Entry.config.Site = site
	catalog.snapshot.Site = site
	task, err := s.NewTask(RuleRequest{URL: "https://example.com/list"}, map[string]string{"business": "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Insert(db); err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusProcessing).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("task_id = ?", task.ID).Delete(&models.RuleSchedule{})
		db.Where("site = ?", site).Delete(&models.CrawlerTask{})
	})
	return db, s, catalog, task
}

func scheduleLoad(t *testing.T, s *RuleSchedule, db *gorm.DB, id uint) (scheduleControl, *scheduleState) {
	t.Helper()
	c, state, err := s.readSchedule(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return c, state
}

func scheduleInsertRoot(db *gorm.DB) func(*Task) error {
	return func(root *Task) error { return root.Insert(db) }
}

func scheduleRun(t *testing.T, s *RuleSchedule, ctx context.Context, task *Task, e *Engine, submit func(*Task) error) error {
	t.Helper()
	var version taskRunVersion
	if err := e.db.Model(&models.CrawlerTask{}).Select("repeat", "reprocess").Where("id = ?", task.ID).Take(&version).Error; err != nil {
		t.Fatal(err)
	}
	return s.runSchedule(withTaskRunVersion(ctx, version), task, e, submit)
}

func TestRuleScheduleMySQLRejectsMissingOrStaleClaim(t *testing.T) {
	db, s, catalog, task := scheduleMySQL(t)
	e := &Engine{db: db}
	ctx := context.Background()
	if err := s.FetchHandler(ctx, task, e); !errors.Is(err, ErrRuleScheduleInvalid) {
		t.Fatalf("unclaimed direct call: %v", err)
	}
	for _, version := range []taskRunVersion{{Repeat: 1}, {Reprocess: 1}} {
		if err := s.runSchedule(withTaskRunVersion(ctx, version), task, e, func(*Task) error { t.Fatal("stale claim dispatched"); return nil }); !errors.Is(err, ErrRuleScheduleConflict) {
			t.Fatalf("stale claim %+v: %v", version, err)
		}
	}
	if catalog.snapshots != 0 {
		t.Fatal("stale claim read Current")
	}
	var rows int64
	if err := db.Model(&models.RuleSchedule{}).Where("task_id = ?", task.ID).Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("stale claim allocated state: %d %v", rows, err)
	}
}

func scheduleUpdateTick(t *testing.T, db *gorm.DB, task *Task, seconds int) {
	t.Helper()
	tick := time.Date(2026, 11, 6, 0, 0, seconds, 0, time.UTC)
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("last_repeat_at", tick).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRuleScheduleMySQLLifecycleSnapshotAndBlocking(t *testing.T) {
	db, s, catalog, task := scheduleMySQL(t)
	ctx := context.Background()
	e := &Engine{db: db}
	status, err := s.Status(ctx, e, uint(task.ID))
	if err != nil || status.BatchID != "" {
		t.Fatalf("unstarted status: %+v %v", status, err)
	}
	if err = scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Status(ctx, e, uint(task.ID))
	if err != nil || first.Sequence != 1 || first.RootID == 0 || first.Pending != 1 || first.Complete {
		t.Fatalf("first: %+v %v", first, err)
	}
	if catalog.snapshots != 1 {
		t.Fatalf("publication reads=%d", catalog.snapshots)
	}
	// A later publication is not consulted while this batch is active or failed.
	catalog.publish("v2", 2)
	catalog.snapshot.Site = task.Site
	scheduleUpdateTick(t, db, task, 10)
	for _, state := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing, models.TaskStatusFailed} {
		if err = db.Model(&models.CrawlerTask{}).Where("id = ?", first.RootID).Update("status", state).Error; err != nil {
			t.Fatal(err)
		}
		if err = scheduleRun(t, s, ctx, task, e, func(*Task) error { t.Fatal("active batch dispatched another root"); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if catalog.snapshots != 1 {
		t.Fatal("active batch reread Current")
	}
	// Restart with a new DB connection reads the fixed root rather than defaults.
	reopened, err := gorm.Open(mysql.Open(os.Getenv("PAPA_RULE_SCHEDULE_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := reopened.DB()
	defer raw.Close()
	if current, err := s.Status(ctx, &Engine{db: reopened}, uint(task.ID)); err != nil || current.BatchID != first.BatchID || current.ReleaseID != "release-1" || current.Failed != 1 {
		t.Fatalf("restart: %+v %v", current, err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", first.RootID).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	if err = scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Status(ctx, e, uint(task.ID))
	if err != nil || second.Sequence != 2 || second.BatchID == first.BatchID || second.ReleaseID != "release-2" {
		t.Fatalf("second: %+v %v", second, err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", second.RootID).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	// Handler retry, manual retry and recovery can change Repeat but not the poll tick.
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"repeat": 99, "reprocess": 3}).Error; err != nil {
		t.Fatal(err)
	}
	if err = scheduleRun(t, s, ctx, task, e, func(*Task) error { t.Fatal("same tick advanced batch"); return nil }); err != nil {
		t.Fatal(err)
	}
	if catalog.snapshots != 2 {
		t.Fatalf("same tick reread Current: %d", catalog.snapshots)
	}
	// Re-submitting static controller metadata cannot overwrite the separate state.
	fresh, err := s.NewTask(RuleRequest{URL: "https://example.com/list"}, map[string]string{"business": "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.URL != task.URL {
		t.Fatal("restart control identity changed")
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(submitUpdates(fresh)).Error; err != nil {
		t.Fatal(err)
	}
	_, stored := scheduleLoad(t, s, db, uint(task.ID))
	if stored.payload.Sequence != 2 {
		t.Fatal("static submission lost state")
	}
}

func TestRuleScheduleMySQLEmptyOrphanUnknownAndCorrupt(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	ctx := context.Background()
	e := &Engine{db: db}
	c, old := scheduleLoad(t, s, db, uint(task.ID))
	p, err := s.prepareBatch(ctx, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.commitBatch(ctx, db, c, old, p); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, e, uint(task.ID))
	if err != nil || status.Complete || status.Total != 0 || status.RootID != 0 {
		t.Fatalf("empty: %+v %v", status, err)
	}
	// A missing root with any surviving child is corruption, never a fresh start.
	child := p.Root.task()
	child.URL += "-orphan"
	if err = child.Insert(db); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, e, uint(task.ID)); !errors.Is(err, ErrRuleScheduleCorrupt) {
		t.Fatalf("orphan: %v", err)
	}
	if err = db.Delete(&models.CrawlerTask{}, child.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err = scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(ctx, e, uint(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", status.RootID).Update("status", 9).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, e, uint(task.ID)); !errors.Is(err, ErrRuleScheduleCorrupt) {
		t.Fatalf("unknown status: %v", err)
	}
	if err = db.Model(&models.RuleSchedule{}).Where("task_id = ?", task.ID).Update("payload_json", gorm.Expr("CONCAT(payload_json, ' ')")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, e, uint(task.ID)); !errors.Is(err, ErrRuleScheduleCorrupt) {
		t.Fatalf("corruption: %v", err)
	}
	transaction := db.Begin()
	defer transaction.Rollback()
	if _, err = s.Status(ctx, &Engine{db: transaction}, uint(task.ID)); !errors.Is(err, ErrRuleScheduleInvalid) {
		t.Fatalf("outer transaction: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Status(canceled, e, uint(task.ID)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled status: %v", err)
	}
}

func TestRuleScheduleMySQLConcurrentCASAndStaleControl(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	ctx := context.Background()
	c, old := scheduleLoad(t, s, db, uint(task.ID))
	p, err := s.prepareBatch(ctx, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.commitBatch(ctx, db, c, old, p) }()
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrRuleScheduleConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	c, state := scheduleLoad(t, s, db, uint(task.ID))
	if state.row.Revision != 1 {
		t.Fatal("concurrent execution advanced revision")
	}
	next, err := s.prepareBatch(ctx, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	scheduleUpdateTick(t, db, task, 10)
	if err = s.commitBatch(ctx, db, c, state, next); !errors.Is(err, ErrRuleScheduleConflict) {
		t.Fatalf("stale control: %v", err)
	}
}

type scheduleLostAckPool struct {
	gorm.ConnPool
	armed *atomic.Bool
}

func (p *scheduleLostAckPool) BeginTx(ctx context.Context, o *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.ConnPool.(*sql.DB).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &scheduleLostAckTx{Tx: tx, armed: p.armed}, nil
}

type scheduleLostAckTx struct {
	*sql.Tx
	armed *atomic.Bool
}

var errScheduleLostAck = errors.New("injected schedule COMMIT acknowledgement loss")

func (t *scheduleLostAckTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.armed.Swap(false) {
		return errScheduleLostAck
	}
	return nil
}

func TestRuleScheduleMySQLUnknownCommitAndRootReceiptLoss(t *testing.T) {
	db, s, catalog, task := scheduleMySQL(t)
	ctx := context.Background()
	sqlDB, _ := db.DB()
	var armed atomic.Bool
	pool := &scheduleLostAckPool{ConnPool: sqlDB, armed: &armed}
	wrapped := db.Session(&gorm.Session{NewDB: true})
	wrapped.Config.ConnPool = pool
	wrapped.Statement.ConnPool = pool
	armed.Store(true)
	dispatches := 0
	err := scheduleRun(t, s, ctx, task, &Engine{db: wrapped}, func(*Task) error { dispatches++; return nil })
	if !errors.Is(err, ErrRuleScheduleUnconfirmed) || !errors.Is(err, errScheduleLostAck) || dispatches != 0 {
		t.Fatalf("unknown commit dispatched: %d %v", dispatches, err)
	}
	_, state := scheduleLoad(t, s, db, uint(task.ID))
	if state == nil || state.payload.Sequence != 1 {
		t.Fatal("committed state missing")
	}
	fixed := state.payload.Root.URL
	// The task insert commits but its caller loses the receipt. Retry observes
	// the same root; it does not allocate another batch or read Current again.
	err = scheduleRun(t, s, ctx, task, &Engine{db: db}, func(root *Task) error {
		dispatches++
		if root.URL != fixed {
			t.Fatal("fixed root changed")
		}
		if err := root.Insert(db); err != nil {
			return err
		}
		return errScheduleLostAck
	})
	if !errors.Is(err, errScheduleLostAck) {
		t.Fatal(err)
	}
	if err = scheduleRun(t, s, ctx, task, &Engine{db: db}, func(*Task) error { dispatches++; return nil }); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 || catalog.snapshots != 1 {
		t.Fatalf("replayed batch: dispatches=%d snapshots=%d", dispatches, catalog.snapshots)
	}
	first, err := s.Status(ctx, &Engine{db: db}, uint(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", first.RootID).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	scheduleUpdateTick(t, db, task, 10)
	catalog.publish("v2", 2)
	catalog.snapshot.Site = task.Site
	armed.Store(true)
	err = scheduleRun(t, s, ctx, task, &Engine{db: wrapped}, func(*Task) error { dispatches++; return nil })
	if !errors.Is(err, ErrRuleScheduleUnconfirmed) || dispatches != 1 {
		t.Fatalf("unconfirmed UPDATE dispatched: %d %v", dispatches, err)
	}
	_, next := scheduleLoad(t, s, db, uint(task.ID))
	if next.payload.Sequence != 2 {
		t.Fatal("next state UPDATE did not commit")
	}
	if err = scheduleRun(t, s, ctx, task, &Engine{db: db}, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Status(ctx, &Engine{db: db}, uint(task.ID))
	if err != nil || second.Sequence != 2 || second.ReleaseID != "release-2" || catalog.snapshots != 2 {
		t.Fatalf("update recovery: %+v snapshots=%d %v", second, catalog.snapshots, err)
	}
}
