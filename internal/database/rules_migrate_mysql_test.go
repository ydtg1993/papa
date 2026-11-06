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
}
