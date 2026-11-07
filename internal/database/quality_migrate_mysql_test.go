package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
)

func TestQualityMigrationIsExplicitAndOptIn(t *testing.T) {
	dsn := os.Getenv("PAPA_QUALITY_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_QUALITY_MIGRATION_TEST_DSN not set; real MySQL migration not verified")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_quality_test_migration" {
		t.Fatal("requires dedicated papa_quality_test_migration database")
	}
	cfg := &config.Config{}
	cfg.DB.Driver = "mysql"
	cfg.DB.DSN = dsn
	cfg.DB.LogLevel = config.SQLLogSilent
	cfg.DB.MaxIdleConns = 1
	cfg.DB.MaxOpenConns = 2
	cfg.Quality.Enabled = true
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sqldb, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	tables, err := db.Migrator().GetTables()
	if err != nil || len(tables) != 0 {
		t.Fatalf("requires empty schema; NewDB must not create tables: %v %v", tables, err)
	}
	store, err := rulequality.New(db)
	if err != nil {
		t.Fatal(err)
	}
	observation := rulequality.Observation{ID: "migration-probe", Scope: rulequality.Scope{Site: "migration", Stage: "catalog", RuleID: "migration-rule", Version: "v1"}, RequestSHA256: strings.Repeat("a", 64), Operation: "source"}
	if _, err := store.Record(context.Background(), observation); err == nil {
		t.Fatal("store implicitly migrated")
	}
	tables, err = db.Migrator().GetTables()
	if err != nil || len(tables) != 0 {
		t.Fatalf("constructor/Record performed DDL: %v %v", tables, err)
	}
	all := FrameworkModels(cfg)
	defer func() {
		for i := len(all) - 1; i >= 0; i-- {
			if err := db.Migrator().DropTable(all[i].Value); err != nil {
				t.Errorf("clean owned migration table %s: %v", all[i].Name, err)
			}
		}
	}()
	cfg.Quality.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	for _, table := range []any{&models.RuleObservation{}, &models.RuleQualityReport{}, &models.RuleReanalysisRequest{}} {
		if db.Migrator().HasTable(table) {
			t.Fatal("disabled quality created a table")
		}
	}
	cfg.Quality.Enabled = true
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db, cfg); err != nil {
			t.Fatalf("explicit migration pass %d: %v", pass, err)
		}
	}
	for _, m := range all {
		if !db.Migrator().HasTable(m.Value) {
			t.Fatalf("missing %s", m.Name)
		}
	}
	if db.Migrator().HasTable(&models.AgentRun{}) || db.Migrator().HasTable(&models.RuleVersion{}) {
		t.Fatal("quality enabled unrelated features")
	}
	saved, err := store.Record(context.Background(), observation)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Quality.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	w, err := store.Query(context.Background(), rulequality.Query{Scope: saved.Scope, From: saved.CreatedAt.Add(-time.Second), Until: saved.CreatedAt.Add(time.Second), Limit: 10})
	if err != nil || len(w.Observations) != 1 {
		t.Fatalf("disabled migration removed observations: %+v %v", w, err)
	}
	var columns []struct {
		TableName     string
		ColumnName    string
		DataType      string
		ColumnComment string
	}
	if err := db.Raw("SELECT TABLE_NAME,COLUMN_NAME,DATA_TYPE,COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN ('crawler_rule_observations','crawler_rule_quality_reports','crawler_rule_reanalysis_requests')").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatal("quality columns not inspected")
	}
	for _, column := range columns {
		if column.ColumnComment == "" {
			t.Fatalf("missing SQL comment: %+v", column)
		}
		if column.ColumnName == "id" || column.ColumnName == "operation_id" || column.ColumnName == "report_id" || column.ColumnName == "run_id" {
			if column.DataType != "varbinary" {
				t.Fatalf("nonbinary identity: %+v", column)
			}
		}
	}
	for _, name := range []string{"idx_quality_scope", "idx_quality_prune"} {
		if !db.Migrator().HasIndex(&models.RuleObservation{}, name) {
			t.Fatalf("missing observation query index %s", name)
		}
	}
}
