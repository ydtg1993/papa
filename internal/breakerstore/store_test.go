package breakerstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/incident"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Only a freshly created random schema is migrated. The configured business
// database is never opened; testdb owns cleanup of the schema it created.
func isolatedMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PAPA_BREAKER_TEST_DSN")
	if dsn == "" {
		path := os.Getenv("PAPA_RUNTIME_TEST_CONFIG")
		if path == "" {
			t.Skip("PAPA_BREAKER_TEST_DSN/PAPA_RUNTIME_TEST_CONFIG not set")
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
	c.Loc, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 5 * time.Second
	server, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test server")
	}
	t.Cleanup(func() { _ = server.Close() })
	name := "papa_breaker_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		t.Fatal("cannot open isolated breaker database")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBreakerStoreMySQLLifecycleRestartAndRevision(t *testing.T) {
	db := isolatedMySQL(t)
	ctx := context.Background()
	store := New(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	next := now.Add(time.Hour)
	l := incident.Lifecycle{Capture: incident.Capture{ID: "event-one", Site: "synthetic", At: now, Failures: 1}, Stage: "detail", State: "cooldown", Revision: 1, Epoch: 4, StartedAt: now, UpdatedAt: now, NextProbeAt: next, Deadline: next.Add(time.Hour), Evidence: []incident.Sample{{Error: "HTTP 503 token=hidden-value"}}, Timeline: []incident.Transition{{At: now, To: "cooldown", Action: "initial"}}}
	l.Checks = []incident.StageCheck{{Stage: "detail", State: "passed", Round: 1, TaskSuccesses: 1}, {Stage: "list", State: "failed", Round: 1, Detail: "token=hidden-value"}}
	l.Timeline = append(l.Timeline, incident.Transition{At: now, Stage: "list", Round: 1, Action: "stage_probe_failed", Failures: 1, NextAttemptAt: next})
	if err := store.Save(ctx, snapshotFixture(l)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, snapshotFixture(l)); err != nil {
		t.Fatal("idempotent retry", err)
	}
	restarted := New(db)
	matched, lookupErr := restarted.List(ctx, "synthetic", 100)
	if lookupErr != nil || len(matched) != 1 || matched[0].Capture.ID != l.Capture.ID {
		t.Fatalf("ID lookup: %+v %v", matched, lookupErr)
	}
	active, err := restarted.LoadRuntime(ctx)
	if err != nil || len(active) != 1 || !active[0].NextProbeAt.Equal(next) || active[0].Epoch != 4 {
		t.Fatalf("restore: %+v %v", active, err)
	}
	if len(active[0].Checks) != 2 || active[0].Checks[1].State != "failed" || active[0].Timeline[1].Stage != "list" || !active[0].Timeline[1].NextAttemptAt.Equal(next) {
		t.Fatal("lost per-stage recovery timeline")
	}
	var row models.BreakerIncident
	if err := db.First(&row, "id = ?", l.Capture.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.NextProbeAt == nil || !row.NextProbeAt.Equal(next) || !row.StartedAt.Equal(now) {
		t.Fatalf("UTC timestamps changed with non-UTC driver: %+v", row)
	}
	if strings.Contains(string(row.Payload), "hidden-value") {
		t.Fatal("persisted credential")
	}
	l.State = "manual"
	l.Revision = 2
	l.Conclusion = "需要人工检查"
	l.NextProbeAt = time.Time{}
	if err := store.Save(ctx, snapshotFixture(l)); err != nil {
		t.Fatal(err)
	}
	active, err = restarted.LoadRuntime(ctx)
	if err != nil || len(active) != 1 || active[0].State != "manual" {
		t.Fatal("manual pause must restore", active, err)
	}
	old := l
	old.Revision = 1
	if err := store.Save(ctx, snapshotFixture(old)); !errors.Is(err, ErrStale) {
		t.Fatal("accepted stale snapshot", err)
	}
	conflict := l
	conflict.Conclusion = "conflict"
	if err := store.Save(ctx, snapshotFixture(conflict)); !errors.Is(err, ErrStale) {
		t.Fatal("accepted conflicting revision", err)
	}
	conflict = l
	conflict.Revision = 3
	conflict.Stage = "other"
	if err := store.Save(ctx, snapshotFixture(conflict)); !errors.Is(err, ErrStale) {
		t.Fatal("changed event ownership", err)
	}
	l.Stage = "" // A cross-stage outbreak widens protection to the whole site.
	l.Revision = 3
	if err := store.Save(ctx, snapshotFixture(l)); err != nil {
		t.Fatal("failed to widen stage scope", err)
	}
	active, err = restarted.LoadRuntime(ctx)
	if err != nil || len(active) != 1 || active[0].Stage != "" {
		t.Fatal("widened scope did not survive restart", active, err)
	}
	conflict = l
	conflict.Revision = 4
	conflict.Stage = "detail"
	if err := store.Save(ctx, snapshotFixture(conflict)); !errors.Is(err, ErrStale) {
		t.Fatal("allowed scope shrinking", err)
	}
	l.State = "recovered"
	l.Revision = 4
	l.FinalAt = now
	l.Conclusion = "恢复已验证"
	l.CountersResetAt = now
	if err := store.Save(ctx, snapshotFixture(l)); err != nil {
		t.Fatal(err)
	}
	active, err = restarted.LoadRuntime(ctx)
	if err != nil || len(active) != 1 || active[0].Control.Active() {
		t.Fatal("closed event still active", active, err)
	}
	reports, err := restarted.List(ctx, "synthetic", 100)
	if err != nil || len(reports) != 1 || reports[0].Conclusion != "恢复已验证" || !reports[0].CountersResetAt.Equal(now) {
		t.Fatal("lost final report", reports, err)
	}
	reports, err = restarted.List(ctx, "other", 100)
	if err != nil || len(reports) != 0 {
		t.Fatal("site isolation", reports, err)
	}
}

func TestDecodeRejectsMetadataMismatch(t *testing.T) {
	_, err := decode([]models.BreakerIncident{{ID: "one", Payload: []byte(`{"capture":{"id":"other"}}`)}})
	if err == nil {
		t.Fatal("accepted corrupt recovery data")
	}
	if err := New(nil).Save(context.Background(), snapshotFixture(incident.Lifecycle{})); err == nil {
		t.Fatal("nil database silently accepted")
	}
}
