package database

import (
	"os"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/models"
)

// TestRulesMigrationMySQL uses an explicitly supplied empty disposable database.
// It never creates or drops a database and only removes tables it created.
func TestRulesMigrationMySQL(t *testing.T) {
	dsn := os.Getenv("PAPA_RULE_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_RULE_MIGRATION_TEST_DSN to an empty papa_rule_test_migration database")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable migration DSN:", err)
	}
	if parsed.DBName != "papa_rule_test_migration" {
		t.Fatal("migration integration test requires the dedicated papa_rule_test_migration database")
	}
	cfg := &config.Config{}
	cfg.DB.Driver = "mysql"
	cfg.DB.DSN = dsn
	cfg.DB.LogLevel = config.SQLLogSilent
	cfg.DB.MaxIdleConns = 1
	cfg.DB.MaxOpenConns = 2
	cfg.Rules.Enabled = true
	cfg.Rules.SampleDir = t.TempDir()
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	// Merely opening an enabled configuration must leave the empty schema alone.
	tables, err := db.Migrator().GetTables()
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 0 {
		t.Fatalf("requires an empty test database; NewDB must not implicitly migrate: %v", tables)
	}
	allModels := FrameworkModels(cfg)
	defer func() {
		for i := len(allModels) - 1; i >= 0; i-- {
			if err := db.Migrator().DropTable(allModels[i].Value); err != nil {
				t.Errorf("clean test table %s: %v", allModels[i].Name, err)
			}
		}
	}()

	cfg.Rules.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal("migrate with rules disabled:", err)
	}
	for _, model := range allModels {
		if got, want := db.Migrator().HasTable(model.Value), !strings.HasPrefix(model.Name, "crawler_rule_"); got != want {
			t.Fatalf("rules disabled: table %s exists=%v, want %v", model.Name, got, want)
		}
	}

	cfg.Rules.Enabled = true
	for pass := 1; pass <= 2; pass++ {
		if err := Migrate(db, cfg); err != nil {
			t.Fatalf("explicit enabled migration pass %d: %v", pass, err)
		}
	}
	for _, model := range allModels {
		if !db.Migrator().HasTable(model.Value) {
			t.Fatalf("enabled migration missing %s", model.Name)
		}
	}
	for _, name := range []string{"uidx_rule_release_operation", "uidx_rule_release_generation"} {
		ok, err := HasIndexNamed(db, "crawler_rule_releases", name)
		if err != nil || !ok {
			t.Fatalf("release constraint %s: exists=%v error=%v", name, ok, err)
		}
	}

	t.Run("existing_rule_schema_adds_schedule_without_data_loss", func(t *testing.T) {
		// All tables here belong to this test. Remove only the new schedule table
		// to reproduce an already-migrated rules database from before this feature.
		if err := db.Migrator().DropTable(&models.RuleSchedule{}); err != nil {
			t.Fatal(err)
		}
		probe := models.RuleSite{Site: "schedule-migration-probe", Generation: 7, CurrentReleaseID: "preserved-release"}
		if err := db.Create(&probe).Error; err != nil {
			t.Fatal(err)
		}
		cfg.Rules.Enabled = false
		if err := Migrate(db, cfg); err != nil {
			t.Fatal("disabled migration of existing rules database:", err)
		}
		if db.Migrator().HasTable(&models.RuleSchedule{}) {
			t.Fatal("disabled rules created the new schedule table")
		}
		cfg.Rules.Enabled = true
		if err := Migrate(db, cfg); err != nil {
			t.Fatal("add schedule to existing rules database:", err)
		}
		if !db.Migrator().HasTable(&models.RuleSchedule{}) {
			t.Fatal("explicit migration did not add the schedule table")
		}
		var restored models.RuleSite
		if err := db.Where("site = ?", probe.Site).Take(&restored).Error; err != nil {
			t.Fatal(err)
		}
		if restored != probe {
			t.Fatalf("migration changed existing rule site: %+v", restored)
		}
	})

	// The snapshot can exceed MySQL TEXT's 64 KiB limit and the revision can
	// exceed uint32. Repeated and subsequently disabled migrations retain both.
	payload := `{"root":"` + strings.Repeat("x", 70*1024) + `"}`
	schedule := models.RuleSchedule{TaskID: 91, Revision: 1<<40 + 7, PayloadSHA256: models.UrlHash(payload), PayloadJSON: payload}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal("persist large schedule snapshot:", err)
	}
	for pass := 1; pass <= 2; pass++ {
		if err := Migrate(db, cfg); err != nil {
			t.Fatalf("repeat schedule migration pass %d: %v", pass, err)
		}
	}
	assertSchedule := func() {
		t.Helper()
		var restored models.RuleSchedule
		if err := db.Where("task_id = ?", schedule.TaskID).Take(&restored).Error; err != nil {
			t.Fatal(err)
		}
		if restored != schedule {
			t.Fatalf("migration changed schedule snapshot: task=%d revision=%d digest=%q bytes=%d", restored.TaskID, restored.Revision, restored.PayloadSHA256, len(restored.PayloadJSON))
		}
	}
	assertSchedule()
	var scheduleColumns []struct {
		ColumnName    string
		DataType      string
		ColumnType    string
		ColumnKey     string
		Extra         string
		ColumnComment string
	}
	if err := db.Raw("SELECT COLUMN_NAME,DATA_TYPE,COLUMN_TYPE,COLUMN_KEY,EXTRA,COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='crawler_rule_schedules'").Scan(&scheduleColumns).Error; err != nil {
		t.Fatal(err)
	}
	if len(scheduleColumns) != 4 {
		t.Fatalf("schedule columns = %d, want 4", len(scheduleColumns))
	}
	for _, column := range scheduleColumns {
		if column.ColumnComment == "" {
			t.Fatalf("missing schedule SQL comment: %+v", column)
		}
		switch column.ColumnName {
		case "task_id":
			if column.ColumnKey != "PRI" || strings.Contains(column.Extra, "auto_increment") {
				t.Fatalf("schedule must use the supplied control task ID: %+v", column)
			}
		case "revision":
			if column.DataType != "bigint" || !strings.Contains(column.ColumnType, "unsigned") {
				t.Fatalf("schedule revision must preserve uint64: %+v", column)
			}
		case "payload_sha256":
			if column.ColumnType != "char(64)" {
				t.Fatalf("unexpected schedule digest type: %+v", column)
			}
		case "payload_json":
			if column.DataType != "longtext" {
				t.Fatalf("unexpected schedule payload type: %+v", column)
			}
		default:
			t.Fatalf("unexpected schedule column: %+v", column)
		}
	}

	t.Run("existing_sample_metadata_text_is_widened_without_data_loss", func(t *testing.T) {
		// Simulate a schema created by the earlier third-batch implementation.
		// This dedicated database is empty at test entry and cleaned at exit.
		if err := db.Exec("ALTER TABLE `crawler_rule_samples` MODIFY COLUMN `metadata_json` TEXT NOT NULL COMMENT 'Durable rule MetadataJSON'").Error; err != nil {
			t.Fatal(err)
		}
		probe := models.RuleSample{SampleID: "migration-probe", Digest: strings.Repeat("a", 64), MetadataJSON: `{"preserved":true}`, Operator: "test", Source: "migration"}
		if err := db.Create(&probe).Error; err != nil {
			t.Fatal(err)
		}
		if err := Migrate(db, cfg); err != nil {
			t.Fatal("explicit migration of existing sample metadata:", err)
		}
		var columnType string
		if err := db.Raw("SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'crawler_rule_samples' AND COLUMN_NAME = 'metadata_json'").Scan(&columnType).Error; err != nil {
			t.Fatal(err)
		}
		if columnType != "longtext" {
			t.Fatalf("metadata column type = %q, want longtext", columnType)
		}
		var restored models.RuleSample
		if err := db.Where("sample_id = ?", probe.SampleID).Take(&restored).Error; err != nil {
			t.Fatal(err)
		}
		if restored.MetadataJSON != probe.MetadataJSON || restored.Digest != probe.Digest {
			t.Fatalf("migration changed existing sample: %+v", restored)
		}
	})

	// Disabling the feature later must not delete durable versions or history.
	cfg.Rules.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	for _, model := range allModels {
		if !db.Migrator().HasTable(model.Value) {
			t.Fatalf("disabling rules deleted %s", model.Name)
		}
	}
	assertSchedule()
}
