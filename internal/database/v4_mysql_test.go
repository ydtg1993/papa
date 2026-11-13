package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The supplied database name is discarded before connecting. Only an exclusive
// CREATE grants ownership; cleanup checks that this test's database is absent.
func v4MySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PAPA_V4_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_V4_TEST_DSN not set")
	}
	c, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test connection")
	}
	c.DBName = ""
	c.ParseTime = true
	c.Timeout = 5 * time.Second
	server, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test server")
	}
	t.Cleanup(func() { _ = server.Close() })
	name := "papa_v4_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup, err := testdb.Create(ctx, server, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("exclusive database: %s", name)
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
			return
		}
		var n int
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n); err != nil || n != 0 {
			t.Errorf("cannot confirm test database cleanup: count=%d", n)
		} else {
			t.Logf("cleanup verified: %s", name)
		}
	})
	c.DBName = name
	db, err := gorm.Open(mysql.Open(c.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open exclusive test database")
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var version string
	if err := db.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL version: %s", version)
	workCtx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(stop)
	return db.WithContext(workCtx)
}

func TestV4MySQLFreshRepeatMigrationAndTaskRound(t *testing.T) {
	db := v4MySQL(t)
	cfg := &config.Config{}
	cfg.TaskFlow.Enabled, cfg.Server.OperationLog = true, true
	if err := CheckTaskSchema(db); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(db, cfg); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, model := range FrameworkModels(cfg) {
		if !db.Migrator().HasTable(model.Value) {
			t.Fatal("missing table", model.Name)
		}
	}
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/item", IdentityType: "id", IdentityValue: "resource"}
	if err := taskstore.Insert(db, &row); err != nil {
		t.Fatal(err)
	}
	duplicate := row
	duplicate.ID = 0
	var duplicateErr *driver.MySQLError
	if err := taskstore.Insert(db, &duplicate); !errors.As(err, &duplicateErr) || duplicateErr.Number != 1062 {
		t.Fatal("expected identity unique constraint violation", err)
	}
	if r := taskstore.Claim(db, row.ID); r.Error != nil || r.RowsAffected != 1 {
		t.Fatalf("claim: %v rows=%d", r.Error, r.RowsAffected)
	}
	if r := taskstore.Claim(db, row.ID); r.Error != nil || r.RowsAffected != 0 {
		t.Fatal("duplicate claim accepted", r.Error)
	}
	updates := map[string]any{"status": models.TaskStatusSuccess, "error": ""}
	if err := taskstore.Complete(db, int(row.ID), taskstore.RunVersion{}, models.TaskStatusSuccess, updates); err != nil {
		t.Fatal(err)
	}
	if err := taskstore.Complete(db, int(row.ID), taskstore.RunVersion{}, models.TaskStatusSuccess, updates); err != nil {
		t.Fatal("completion confirmation", err)
	}
	if r := taskstore.RetryTask(db, row.ID, models.TaskStatusSuccess, taskstore.RunVersion{}, 0); r.Error != nil || r.RowsAffected != 1 {
		t.Fatal("versioned retry", r.Error)
	}
	if r := taskstore.RetryTask(db, row.ID, models.TaskStatusSuccess, taskstore.RunVersion{}, 0); r.Error != nil || r.RowsAffected != 0 {
		t.Fatal("stale retry accepted", r.Error)
	}
	var got models.CrawlerTask
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.IdentityHash != row.IdentityHash || got.IdentityValue != "resource" || got.Status != models.TaskStatusPending || got.Reprocess != 1 || got.CompensationVersion != 1 {
		t.Fatal("task round changed identity or lost versions")
	}
	if err := Migrate(db, cfg); err != nil {
		t.Fatal("migration with current data", err)
	}
	var afterMigration models.CrawlerTask
	if err := db.First(&afterMigration, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, afterMigration) {
		t.Fatal("repeat migration changed current task data")
	}
	if err := CheckTaskSchema(db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckTaskSchema(db.WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatal("schema cancellation lost", err)
	}
}

func TestV4MySQLRejectsOldTablesWithoutWrites(t *testing.T) {
	for _, tc := range []struct{ name, columns, values string }{
		{"missing_identity", "url TEXT", "'old-url'"},
		{"old_column", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64), url_hash CHAR(64)", "'id','resource',REPEAT('a',64),REPEAT('b',64)"},
		{"old_key", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64), idempotency_key TEXT", "'id','resource',REPEAT('a',64),'old-key'"},
		{"old_hash", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64), idempotency_hash CHAR(64)", "'id','resource',REPEAT('a',64),REPEAT('b',64)"},
		{"old_index", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64), KEY idx_idempotency_hash(identity_hash)", "'id','resource',REPEAT('a',64)"},
		{"old_url_index", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64), KEY idx_site_stage_url_hash(identity_hash)", "'id','resource',REPEAT('a',64)"},
		{"null_type", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "NULL,'resource',REPEAT('a',64)"},
		{"invalid_type", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "'other','resource',REPEAT('a',64)"},
		{"empty_hash", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "'id','resource',''"},
		{"null_hash", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "'id','resource',NULL"},
		{"empty_value", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "'id','',REPEAT('a',64)"},
		{"null_value", "identity_type VARCHAR(8), identity_value TEXT, identity_hash CHAR(64)", "'id',NULL,REPEAT('a',64)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := v4MySQL(t)
			if err := db.Exec("CREATE TABLE crawler_tasks (" + tc.columns + ")").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO crawler_tasks VALUES (" + tc.values + ")").Error; err != nil {
				t.Fatal(err)
			}
			var before, after []map[string]any
			var beforeRows, afterRows []map[string]any
			if err := db.Raw("SELECT * FROM crawler_tasks").Scan(&beforeRows).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Raw("SHOW CREATE TABLE crawler_tasks").Scan(&before).Error; err != nil {
				t.Fatal(err)
			}
			if err := CheckTaskSchema(db); err == nil {
				t.Fatal("startup accepted old table")
			}
			if err := Migrate(db, &config.Config{}); err == nil {
				t.Fatal("migration accepted old table")
			} else {
				t.Logf("rejected: %v", err)
			}
			if err := db.Raw("SHOW CREATE TABLE crawler_tasks").Scan(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejection modified schema")
			}
			if err := db.Raw("SELECT * FROM crawler_tasks").Scan(&afterRows).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeRows, afterRows) {
				t.Fatal("rejection modified stored values")
			}
			var tables, rows int64
			if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("crawler_tasks").Count(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if tables != 1 || rows != 1 {
				t.Fatal("rejection wrote tables or rows")
			}
		})
	}
}

func TestV4MySQLIdentityStartupScale(t *testing.T) {
	if os.Getenv("PAPA_V4_SCALE_TEST") != "1" {
		t.Skip("PAPA_V4_SCALE_TEST=1 required for million-row synthetic fixture")
	}
	db := v4MySQL(t)
	if err := Migrate(db, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	seed := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/item", IdentityType: "id", IdentityValue: "1"}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatal(err)
	}
	n := 1
	for _, target := range []int{10000, 100000, 1000000} {
		start := time.Now()
		for n < target {
			add := n
			if add > 10000 {
				add = 10000
			}
			if add > target-n {
				add = target - n
			}
			q := `INSERT INTO crawler_tasks (site,stage,url,identity_type,identity_value,identity_hash)
SELECT 'synthetic','detail','https://example.invalid/item','id',CAST(CAST(identity_value AS UNSIGNED)+? AS CHAR),
SHA2(CONCAT('["papa-task-v1","synthetic","detail","id","',CAST(identity_value AS UNSIGNED)+?,'"]'),256)
FROM crawler_tasks ORDER BY id LIMIT ?`
			if err := db.Exec(q, n, n, add).Error; err != nil {
				t.Fatal(err)
			}
			n += add
		}
		var count int64
		if err := db.Model(&models.CrawlerTask{}).Count(&count).Error; err != nil || count != int64(target) {
			t.Fatalf("fixture count=%d: %v", count, err)
		}
		t.Logf("fixture rows=%d growth=%s", n, time.Since(start))
		for i := 0; i < 3; i++ {
			start = time.Now()
			if err := CheckTaskSchema(db); err != nil {
				t.Fatal(err)
			}
			t.Logf("startup rows=%d run=%d duration=%s", n, i+1, time.Since(start))
		}
	}
	// Reject an invalid identity at either end without changing task data.
	for _, order := range []string{"id ASC", "id DESC"} {
		var row models.CrawlerTask
		if err := db.Order(order).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		identity, err := core.NormalizeTaskIdentity(row.Site, row.Stage, row.IdentityValue, row.URL)
		if err != nil || identity.Hash != row.IdentityHash {
			t.Fatal("fixture hash differs from current writer")
		}
		if err := db.Model(&row).Update("identity_type", "").Error; err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := CheckTaskSchema(db); err == nil {
			t.Fatal("invalid large-table row accepted")
		}
		t.Logf("startup invalid=%s rows=%d duration=%s", order, n, time.Since(start))
		var invalid int64
		start = time.Now()
		if err := db.Table("crawler_tasks").Where("identity_hash IS NULL OR identity_hash = '' OR identity_type IS NULL OR identity_type NOT IN ('id', 'url') OR identity_value IS NULL OR identity_value = ''").Count(&invalid).Error; err != nil || invalid != 1 {
			t.Fatalf("baseline invalid=%d: %v", invalid, err)
		}
		t.Logf("baseline full-count invalid=%s rows=%d duration=%s", order, n, time.Since(start))
		if err := db.Model(&row).Update("identity_type", "id").Error; err != nil {
			t.Fatal(err)
		}
	}
}
