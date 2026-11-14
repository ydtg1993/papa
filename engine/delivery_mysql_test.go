//go:build integration

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Tests mutate only a generated disposable database on a verified loopback
// server. Never accept an application database or a remote server as input.
func deliveryMySQLDB(t *testing.T) *gorm.DB {
	t.Helper()
	raw := os.Getenv("CRAWLER_TEST_MYSQL_DSN")
	if raw == "" {
		t.Skip("set CRAWLER_TEST_MYSQL_DSN to a local server-only DSN")
	}
	dsn, err := drivermysql.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	host, _, err := net.SplitHostPort(dsn.Addr)
	if err != nil || dsn.Net != "tcp" || dsn.DBName != "" || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		t.Fatal("test requires loopback TCP server-only DSN")
	}
	opts := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	server, err := gorm.Open(mysql.Open(dsn.FormatDSN()), opts)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("papa_delivery_test_%d", time.Now().UnixNano())
	if err := server.Exec("CREATE DATABASE `" + name + "`").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(name, "papa_delivery_test_") {
			t.Error("refusing unexpected cleanup database")
			return
		}
		if err := server.Exec("DROP DATABASE `" + name + "`").Error; err != nil {
			t.Error(err)
		}
		conn, _ := server.DB()
		_ = conn.Close()
	})
	dsn.DBName = name
	dsn.ParseTime = true
	db, err := gorm.Open(mysql.Open(dsn.FormatDSN()), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn, _ := db.DB(); _ = conn.Close() })
	if err := db.AutoMigrate(&models.CrawlerTask{}); err != nil {
		t.Fatal(err)
	}
	return db
}

type deliveryMySQLFetcher struct {
	stage    string
	attempts int
	fn       func(context.Context, *Task, *Engine) error
}

func (f *deliveryMySQLFetcher) GetStage() string { return f.stage }
func (f *deliveryMySQLFetcher) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	return f.fn(ctx, task, e)
}

func deliveryMySQLEngine(t *testing.T, db *gorm.DB, paused bool, stages ...*deliveryMySQLFetcher) *Engine {
	t.Helper()
	l := logrus.New()
	l.SetOutput(io.Discard)
	e := NewEngine(db, &config.Config{}, &loggers.LoggerSet{Sys: l, Engine: l, Fetcher: l, Monitor: l, DB: l})
	e.SetSite(core.Site{Key: "synthetic", StartPaused: paused})
	e.SetSiteBreaker("synthetic", struct {
		Enabled   bool
		Window    time.Duration
		Threshold int
	}{})
	for _, f := range stages {
		cfg := StageConfig{Site: "synthetic", StrictFlow: true, Entry: true, WorkerCount: 1, QueueSize: 1, MaxAttempts: 1}
		if f.attempts > 0 {
			cfg.MaxAttempts = f.attempts
		}
		if f.stage == "source" {
			cfg.Downstream = []string{"sink"}
		}
		e.AddStage(f.stage, cfg, f, nil)
	}
	t.Cleanup(func() {
		if drained, _ := e.Stop(3 * time.Second); !drained {
			t.Error("delivery engine did not stop")
		}
	})
	e.ApplyRegisterStage()
	return e
}

func deliveryEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery condition did not become true")
}

func deliveryRows(t *testing.T, db *gorm.DB) []models.CrawlerTask {
	t.Helper()
	var rows []models.CrawlerTask
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestMySQLDeliveryRecoveryRearmsAfterOldOwnerReleases(t *testing.T) {
	db := deliveryMySQLDB(t)
	var calls atomic.Int32
	e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { calls.Add(1); return nil }})
	task := &Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/rearmed"}
	if err := e.SubmitEntryTask(task); err != nil {
		t.Fatal(err)
	}
	if !e.deliveryState().Acquire(task.ID) {
		t.Fatal("cannot simulate old worker owner")
	}
	e.ResumeSite("synthetic")
	if err := e.recoverPendingStage("synthetic:sink"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("executed while old owner exists")
	}
	e.deliveryState().Release(task.ID)
	deliveryEventually(t, func() bool { return deliveryRows(t, db)[0].Status == models.TaskStatusSuccess })
	if calls.Load() != 1 {
		t.Fatalf("executions=%d", calls.Load())
	}
}

func TestMySQLDeliveryRejectsExistingIDFromWrongStage(t *testing.T) {
	db := deliveryMySQLDB(t)
	noop := func(context.Context, *Task, *Engine) error { return nil }
	e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "source", fn: noop}, &deliveryMySQLFetcher{stage: "sink", fn: noop})
	row := &Task{Site: "synthetic", Stage: "source", URL: "https://fixture.invalid/original"}
	if err := e.SubmitEntryTask(row); err != nil {
		t.Fatal(err)
	}
	parent := &Task{ID: 999, Site: "synthetic", Stage: "source"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	child := &Task{ID: row.ID, Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/forged", Meta: map[string]string{"wrong": "stage"}}
	if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatalf("existing ID bypass: %v", err)
	}
	rows := deliveryRows(t, db)
	if len(rows) != 1 || rows[0].Status != models.TaskStatusPending || len(rows[0].Meta) != 0 && string(rows[0].Meta) != "null" {
		t.Fatalf("wrong-stage row mutated: %+v", rows)
	}
}

func TestMySQLDeliveryRejectsCrossStageKeyWithWarmDedupCache(t *testing.T) {
	db := deliveryMySQLDB(t)
	noop := func(context.Context, *Task, *Engine) error { return nil }
	e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "source", fn: noop}, &deliveryMySQLFetcher{stage: "sink", fn: noop})
	root := &Task{Site: "synthetic", Stage: "source", URL: "https://fixture.invalid/key", IdempotencyKey: "shared-key"}
	if err := e.SubmitEntryTask(root); err != nil {
		t.Fatal(err)
	}
	parent := &Task{ID: root.ID, Site: "synthetic", Stage: "source"}
	ctx, release := e.bindTaskExecution(context.Background(), parent)
	defer release()
	for _, batch := range []bool{false, true} {
		child := &Task{PID: root.ID, Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/child", IdempotencyKey: "shared-key"}
		var err error
		if batch {
			err = e.SubmitChildren(ctx, parent, child)
		} else {
			err = e.SubmitTaskContext(ctx, child)
		}
		if !errors.Is(err, ErrTaskFlow) {
			t.Fatalf("warm-cache bypass batch=%v: %v", batch, err)
		}
	}
	if len(deliveryRows(t, db)) != 1 {
		t.Fatal("rejected key added rows")
	}
}

func TestMySQLDeliveryPausedAcceptResumeAndNoAdmissionUpdates(t *testing.T) {
	db := deliveryMySQLDB(t)
	var updates atomic.Int64
	if err := db.Callback().Update().Before("gorm:update").Register("test:count_updates", func(*gorm.DB) { updates.Add(1) }); err != nil {
		t.Fatal(err)
	}
	seen := make(chan int, 4)
	e := deliveryMySQLEngine(t, db, true, &deliveryMySQLFetcher{stage: "sink", fn: func(_ context.Context, task *Task, _ *Engine) error { seen <- task.ID; return nil }})
	baseline := updates.Load()
	for i := 0; i < 3; i++ {
		if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: fmt.Sprintf("https://fixture.invalid/%d", i), Repeatable: i == 0, Meta: map[string]string{"seed": "entry", "cursor": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	// MySQL JSON adds spaces; semantic equality must prevent a metadata UPDATE
	// when repeatable startup entries are submitted again while still pending.
	for i := 0; i < 3; i++ {
		if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/0", Repeatable: true, Meta: map[string]string{"cursor": "v", "seed": "entry"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(deliveryRows(t, db)) != 3 {
		t.Fatal("accepted paused tasks missing")
	}
	select {
	case <-seen:
		t.Fatal("paused task executed")
	case <-time.After(100 * time.Millisecond):
	}
	if updates.Load() != baseline {
		t.Fatal("paused admission wrote UPDATE")
	}
	e.ResumeSite("synthetic")
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		for _, r := range rows {
			if r.Status != models.TaskStatusSuccess {
				return false
			}
		}
		return len(rows) == 3
	})
	if len(seen) != 3 {
		t.Fatalf("executions=%d", len(seen))
	}
}

func TestMySQLDeliveryBoundedPipelineCanceledSubmissionRetainsChildren(t *testing.T) {
	db := deliveryMySQLDB(t)
	var updates atomic.Int64
	_ = db.Callback().Update().Before("gorm:update").Register("test:count_updates", func(*gorm.DB) { updates.Add(1) })
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	seen := make(chan int, 4)
	sourceDone := make(chan error, 1)
	cancelChild := make(chan context.CancelFunc, 1)
	sink := &deliveryMySQLFetcher{stage: "sink", fn: func(ctx context.Context, task *Task, _ *Engine) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-block:
			seen <- task.ID
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	source := &deliveryMySQLFetcher{stage: "source", fn: func(ctx context.Context, parent *Task, e *Engine) error {
		childCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelChild <- cancel
		var children []*Task
		for i := 0; i < 3; i++ {
			children = append(children, &Task{Site: parent.Site, Stage: "sink", URL: fmt.Sprintf("https://fixture.invalid/child/%d", i)})
		}
		err := e.SubmitChildren(childCtx, parent, children...)
		sourceDone <- err
		return err
	}}
	e := deliveryMySQLEngine(t, db, false, source, sink)
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "source", URL: "https://fixture.invalid/root"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sink not started")
	}
	deliveryEventually(t, func() bool { return len(deliveryRows(t, db)) == 4 })
	baseline := updates.Load()
	select {
	case err := <-sourceDone:
		t.Fatalf("source did not block: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if updates.Load() != baseline {
		t.Fatal("waiting on full queue generated UPDATE")
	}
	(<-cancelChild)()
	select {
	case err := <-sourceDone:
		if err != nil {
			t.Fatalf("durable children rejected on cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("submission did not unblock")
	}
	close(block)
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		for _, r := range rows {
			if r.Status != models.TaskStatusSuccess {
				return false
			}
		}
		return len(rows) == 4
	})
	ids := map[int]bool{}
	for len(seen) > 0 {
		id := <-seen
		if ids[id] {
			t.Fatal("child executed twice")
		}
		ids[id] = true
	}
	if len(ids) != 3 {
		t.Fatalf("delivered child count=%d", len(ids))
	}
}

func TestMySQLDeliveryStopFullQueueThenRestartRecoversAll(t *testing.T) {
	db := deliveryMySQLDB(t)
	started := make(chan struct{}, 1)
	e := deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(ctx context.Context, _ *Task, _ *Engine) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	for i := 0; i < 2; i++ {
		if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: fmt.Sprintf("https://fixture.invalid/stop/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	submitDone := make(chan error, 1)
	go func() {
		submitDone <- e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/stop/2"})
	}()
	deliveryEventually(t, func() bool { return len(deliveryRows(t, db)) == 3 })
	if drained, _ := e.Stop(3 * time.Second); !drained {
		t.Fatal("full queue stop did not drain lifecycle")
	}
	select {
	case <-submitDone:
	case <-time.After(time.Second):
		t.Fatal("blocked sender survived stop")
	}
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/rejected"}); !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("post-stop admission=%v", err)
	}
	// Simulate a crash survivor whose timestamp lies ahead of the new process.
	future := time.Now().Add(time.Hour)
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", deliveryRows(t, db)[0].ID).Updates(map[string]any{"status": models.TaskStatusProcessing, "updated_at": future}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusPending).UpdateColumn("updated_at", future).Error; err != nil {
		t.Fatal(err)
	}
	var executed atomic.Int64
	deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executed.Add(1); return nil }})
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		for _, r := range rows {
			if r.Status != models.TaskStatusSuccess {
				return false
			}
		}
		return len(rows) == 3
	})
	if executed.Load() != 3 {
		t.Fatalf("recovered executions=%d", executed.Load())
	}
}

func TestMySQLDeliveryNotBeforeSurvivesRestart(t *testing.T) {
	db := deliveryMySQLDB(t)
	seen := make(chan time.Time, 2)
	fetcher := &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { seen <- time.Now(); return nil }}
	e := deliveryMySQLEngine(t, db, false, fetcher)
	notBefore := time.Now().Add(1500 * time.Millisecond)
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/delayed", NotBefore: notBefore}); err != nil {
		t.Fatal(err)
	}
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("delayed engine did not stop")
	}
	deliveryMySQLEngine(t, db, false, fetcher)
	select {
	case at := <-seen:
		if at.Before(notBefore.Add(-time.Millisecond)) {
			t.Fatalf("early execution by %v", notBefore.Sub(at))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delayed task lost after restart")
	}
	deliveryEventually(t, func() bool { return deliveryRows(t, db)[0].Status == models.TaskStatusSuccess })
}

func TestMySQLDeliveryTerminalWriteRetriesWithoutRefetch(t *testing.T) {
	db := deliveryMySQLDB(t)
	var failed atomic.Bool
	var attempts atomic.Int64
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_terminal_once", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if ok && values["status"] == models.TaskStatusSuccess && failed.CompareAndSwap(false, true) {
			tx.AddError(errors.New("injected terminal write outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	e := deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { attempts.Add(1); return nil }})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/settle"}); err != nil {
		t.Fatal(err)
	}
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		return len(rows) == 1 && rows[0].Status == models.TaskStatusSuccess
	})
	if !failed.Load() || attempts.Load() != 1 {
		t.Fatalf("injected=%v fetches=%d", failed.Load(), attempts.Load())
	}
}

func TestMySQLDeliveryAmbiguousInsertCommitDoesNotStrandTasks(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprintf("bulk_%v", bulk), func(t *testing.T) {
			db := deliveryMySQLDB(t)
			var injected atomic.Bool
			var executed atomic.Int64
			if err := db.Callback().Create().After("gorm:commit_or_rollback_transaction").Register("test:ambiguous_insert", func(tx *gorm.DB) {
				if tx.Error != nil {
					return
				}
				target := false
				switch records := tx.Statement.Dest.(type) {
				case *models.CrawlerTask:
					target = !bulk && records.Stage == "sink"
				case *[]models.CrawlerTask:
					target = bulk && len(*records) > 0 && (*records)[0].Stage == "sink"
				}
				if target && injected.CompareAndSwap(false, true) {
					tx.AddError(errors.New("injected INSERT commit acknowledgement loss"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			sink := &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executed.Add(1); return nil }}
			source := &deliveryMySQLFetcher{stage: "source", fn: func(ctx context.Context, parent *Task, e *Engine) error {
				var children []*Task
				for i := 0; i < 3; i++ {
					children = append(children, &Task{Site: "synthetic", Stage: "sink", URL: fmt.Sprintf("https://fixture.invalid/ambiguous/%d", i)})
				}
				return e.SubmitChildren(ctx, parent, children...)
			}}
			e := deliveryMySQLEngine(t, db, false, source, sink)
			stage := "sink"
			wantRows, wantExecuted := 1, int64(1)
			if bulk {
				stage = "source"
				wantRows, wantExecuted = 4, 3
			}
			if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: stage, URL: "https://fixture.invalid/ambiguous/root"}); err != nil {
				t.Fatal(err)
			}
			deliveryEventually(t, func() bool {
				rows := deliveryRows(t, db)
				if len(rows) != wantRows {
					return false
				}
				for _, r := range rows {
					if r.Status != models.TaskStatusSuccess {
						return false
					}
				}
				return true
			})
			if !injected.Load() || executed.Load() != wantExecuted {
				t.Fatalf("injected=%v executed=%d want=%d", injected.Load(), executed.Load(), wantExecuted)
			}
		})
	}
}

func TestMySQLDeliveryAmbiguousClaimCommitExecutesExactlyOnce(t *testing.T) {
	db := deliveryMySQLDB(t)
	var injected atomic.Bool
	var executed atomic.Int64
	if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:ambiguous_claim", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Error == nil && ok && values["status"] == models.TaskStatusProcessing && injected.CompareAndSwap(false, true) {
			tx.AddError(errors.New("injected claim commit acknowledgement loss"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	e := deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", fn: func(context.Context, *Task, *Engine) error { executed.Add(1); return nil }})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/ambiguous-claim"}); err != nil {
		t.Fatal(err)
	}
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		return len(rows) == 1 && rows[0].Status == models.TaskStatusSuccess
	})
	if !injected.Load() || executed.Load() != 1 {
		t.Fatalf("injected=%v executed=%d", injected.Load(), executed.Load())
	}
}

func TestMySQLDeliveryNonRetryableFailureRemainsTerminal(t *testing.T) {
	db := deliveryMySQLDB(t)
	var attempts atomic.Int64
	e := deliveryMySQLEngine(t, db, false, &deliveryMySQLFetcher{stage: "sink", attempts: 3, fn: func(context.Context, *Task, *Engine) error {
		attempts.Add(1)
		return WrapNoRetry(errors.New("synthetic structure failure"))
	}})
	if err := e.SubmitEntryTask(&Task{Site: "synthetic", Stage: "sink", URL: "https://fixture.invalid/permanent"}); err != nil {
		t.Fatal(err)
	}
	deliveryEventually(t, func() bool {
		rows := deliveryRows(t, db)
		return len(rows) == 1 && rows[0].Status == models.TaskStatusFailed
	})
	row := deliveryRows(t, db)[0]
	if attempts.Load() != 1 || row.Retry != 0 || !strings.Contains(row.Error, "synthetic structure failure") {
		t.Fatalf("attempts=%d retry=%d error=%s", attempts.Load(), row.Retry, row.Error)
	}
}
