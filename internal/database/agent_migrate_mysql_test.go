package database

import (
	"context"
	"os"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
)

func TestAgentMigrationMySQLExplicitAndOptIn(t *testing.T) {
	dsn := os.Getenv("PAPA_AGENT_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_AGENT_MIGRATION_TEST_DSN to empty papa_agent_test_migration database")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_agent_test_migration" {
		t.Fatal("migration test requires dedicated papa_agent_test_migration database")
	}
	cfg := &config.Config{}
	cfg.DB.Driver, cfg.DB.DSN, cfg.DB.LogLevel = "mysql", dsn, config.SQLLogSilent
	cfg.DB.MaxIdleConns, cfg.DB.MaxOpenConns = 1, 2
	cfg.Rules.Enabled, cfg.Rules.SampleDir, cfg.Agent.Enabled = true, t.TempDir(), true
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
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 0 {
		t.Fatalf("requires empty schema; NewDB must not migrate: %v", tables)
	}
	store, err := agentstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(context.Background(), agentstore.Run{ID: "before-explicit-migration"}); err == nil {
		t.Fatal("store silently migrated tables")
	}
	tables, err = db.Migrator().GetTables()
	if err != nil || len(tables) != 0 {
		t.Fatalf("store performed implicit DDL: %v %v", tables, err)
	}
	all := FrameworkModels(cfg)
	defer func() {
		for i := len(all) - 1; i >= 0; i-- {
			if err := db.Migrator().DropTable(all[i].Value); err != nil {
				t.Errorf("clean owned test table %s: %v", all[i].Name, err)
			}
		}
	}()
	cfg.Agent.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&models.AgentRun{}) || db.Migrator().HasTable(&models.AgentStep{}) {
		t.Fatal("disabled agent created tables")
	}
	cfg.Agent.Enabled = true
	for pass := 0; pass < 2; pass++ {
		if err := Migrate(db, cfg); err != nil {
			t.Fatalf("explicit migration %d: %v", pass, err)
		}
	}
	for _, model := range all {
		if !db.Migrator().HasTable(model.Value) {
			t.Fatalf("missing %s", model.Name)
		}
	}
	r, err := store.Start(context.Background(), agentstore.Run{ID: "kept-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginStep(context.Background(), r.ID, r.Revision, agentstore.Step{Kind: "model"}, `{"calls":1}`); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Enabled = false
	if err := Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), r.ID); err != nil {
		t.Fatalf("disabled migration deleted audit: %v", err)
	}
	var columns []struct {
		TableName  string
		ColumnName string
		DataType   string
	}
	if err := db.Raw("SELECT TABLE_NAME,COLUMN_NAME,DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND ((TABLE_NAME='crawler_agent_runs' AND COLUMN_NAME='id') OR (TABLE_NAME='crawler_agent_steps' AND COLUMN_NAME='run_id'))").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 {
		t.Fatalf("ID column inspection: %+v", columns)
	}
	for _, column := range columns {
		if column.DataType != "varbinary" {
			t.Fatalf("ID must be binary: %+v", column)
		}
	}
}
