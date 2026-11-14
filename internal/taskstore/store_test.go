package taskstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type sqlRecorder struct{ sql []string }

func (r *sqlRecorder) LogMode(logger.LogLevel) logger.Interface    { return r }
func (*sqlRecorder) Info(context.Context, string, ...interface{})  {}
func (*sqlRecorder) Warn(context.Context, string, ...interface{})  {}
func (*sqlRecorder) Error(context.Context, string, ...interface{}) {}
func (r *sqlRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	r.sql = append(r.sql, sql)
}

func dryStore(t *testing.T) (*gorm.DB, *sqlRecorder) {
	t.Helper()
	r := &sqlRecorder{}
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:9)/synthetic", SkipInitializeWithVersion: true}),
		&gorm.Config{DryRun: true, SkipDefaultTransaction: true, DisableAutomaticPing: true, Logger: r})
	if err != nil {
		t.Fatal(err)
	}
	return db, r
}

func assertSQL(t *testing.T, r *sqlRecorder, fragments ...string) string {
	t.Helper()
	if len(r.sql) != 1 {
		t.Fatalf("queries=%v, want exactly one", r.sql)
	}
	sql := r.sql[0]
	for _, fragment := range fragments {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("missing %q in %s", fragment, sql)
		}
	}
	return sql
}

func TestPendingBatchHasScopeKeysetAndBoundedLimit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		want  string
	}{{0, "LIMIT 100"}, {2, "LIMIT 2"}, {100000, "LIMIT 1000"}} {
		db, r := dryStore(t)
		if _, err := PendingBatch(db, context.Background(), "site-a", "detail", 7, tc.limit); err != nil {
			t.Fatal(err)
		}
		sql := assertSQL(t, r, "SELECT * FROM `crawler_tasks`", "site = 'site-a'", "stage = 'detail'", "status = 0", "id > 7", "ORDER BY id", tc.want)
		if strings.Contains(sql, "updated_at") || strings.Contains(sql, "not_before <=") {
			t.Fatalf("pending recovery must not omit deferred/unowned rows: %s", sql)
		}
	}
}

func TestResetOrphansOnlyTransitionsProcessing(t *testing.T) {
	db, r := dryStore(t)
	if err := ResetOrphans(db, context.Background()); err != nil {
		t.Fatal(err)
	}
	sql := assertSQL(t, r, "UPDATE `crawler_tasks` SET", "`status`=0", "WHERE status = 1")
	if strings.Contains(sql, "`not_before`=") || strings.Contains(sql, "`meta`=") {
		t.Fatalf("reset erased durable metadata: %s", sql)
	}
}

func TestSettleIsConditionalAndOnlyFailureAppendsError(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed} {
		db, r := dryStore(t)
		if err := Settle(db, context.Background(), 7, status, errors.New("synthetic failure"), 12*time.Hour); err != nil {
			t.Fatal(err)
		}
		sql := assertSQL(t, r, "UPDATE `crawler_tasks` SET", "WHERE id = 7 AND status = 1", "CASE WHEN repeatable = 1 AND next_repeat_at IS NULL", "COALESCE(NULLIF(repeat_interval, 0), 43200)", "ELSE next_repeat_at END")
		if status == models.TaskStatusFailed {
			if !strings.Contains(sql, "CONCAT(COALESCE(error, ''),") || !strings.Contains(sql, "synthetic failure") {
				t.Fatalf("missing atomic append: %s", sql)
			}
		} else if strings.Contains(sql, "error") {
			t.Fatalf("success overwrote error history: %s", sql)
		}
	}
	db, r := dryStore(t)
	if err := Settle(db, context.Background(), 7, models.TaskStatusPending, nil, 12*time.Hour); err == nil {
		t.Fatal("accepted nonterminal settlement")
	}
	if len(r.sql) != 0 {
		t.Fatalf("invalid settlement wrote SQL: %v", r.sql)
	}
}

func TestClaimScopeChecksPersistedNotBeforeInSameUpdate(t *testing.T) {
	db, r := dryStore(t)
	if err := ClaimScope(db, 7).Updates(map[string]any{"status": models.TaskStatusProcessing, "urgent": false}).Error; err != nil {
		t.Fatal(err)
	}
	assertSQL(t, r, "WHERE (id = 7 AND status = 0) AND (not_before IS NULL OR not_before <=", "`status`=1", "`urgent`=false")
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&models.CrawlerTask{}); err != nil {
		t.Fatal(err)
	}
	field := stmt.Schema.LookUpField("NotBefore")
	if field == nil || field.DBName != "not_before" {
		t.Fatalf("claim predicate no longer matches model: %+v", field)
	}
}

func TestGenerationScopeChecksBothRoundCounters(t *testing.T) {
	db, r := dryStore(t)
	row := &models.CrawlerTask{ID: 7, Repeat: 3, Reprocess: 4}
	if err := GenerationScope(db.Model(&models.CrawlerTask{}), row).Where("status = ?", models.TaskStatusFailed).Updates(map[string]any{"status": models.TaskStatusPending}).Error; err != nil {
		t.Fatal(err)
	}
	assertSQL(t, r, "id = 7 AND `repeat` = 3 AND reprocess = 4", "status = 3")
}

func TestAutomaticRetryScopeNeverBypassesProhibition(t *testing.T) {
	for _, limit := range []int{0, 3} {
		db, r := dryStore(t)
		if err := AutomaticRetryScope(db.Model(&models.CrawlerTask{}), limit).Updates(RetryUpdates(true)).Error; err != nil {
			t.Fatal(err)
		}
		sql := assertSQL(t, r, "status = 3 AND retry_blocked = false", "auto_retry_count + 1", "reprocess + 1")
		if strings.Contains(sql, "auto_retry_count <") != (limit > 0) {
			t.Fatal(sql)
		}
	}
}
