package database

import (
	"database/sql/driver"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"strings"
	"testing"
)

func TestMigrationRejectsPreV4SchemaBeforeWrites(t *testing.T) {
	f := gormfake.New()
	f.SetRow(map[string]driver.Value{"column_name": "url"})
	err := Migrate(gormfake.MustOpen(f), &config.Config{})
	assertSchemaReadOnly(t, f.Written())
	if err == nil || !strings.Contains(err.Error(), "unsupported task schema") {
		t.Fatalf("err=%v", err)
	}
}
func TestMigrationSchemaReadFailureStopsBeforeWrites(t *testing.T) {
	f := gormfake.New()
	f.SetQueryError("information_schema.columns", "schema unavailable")
	if err := Migrate(gormfake.MustOpen(f), &config.Config{}); err == nil {
		t.Fatal("schema error ignored")
	}
}

func TestCurrentTaskColumnsRejectBackfilledOldTable(t *testing.T) {
	columns := map[string]bool{"identity_type": true, "identity_value": true, "identity_hash": true}
	if err := validateTaskColumns(columns); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"url_hash", "idempotency_key", "idempotency_hash"} {
		columns[old] = true
		if err := validateTaskColumns(columns); err == nil {
			t.Fatal("accepted obsolete column", old)
		}
		delete(columns, old)
	}
}
func TestTaskSchemaEmptyDatabaseIsReadOnly(t *testing.T) {
	f := gormfake.New()
	if err := CheckTaskSchema(gormfake.MustOpen(f)); err != nil {
		t.Fatal(err)
	}
	assertSchemaReadOnly(t, f.Written())
}

func assertSchemaReadOnly(t *testing.T, sql string) {
	t.Helper()
	for _, line := range strings.Split(sql, "\n") {
		if line != "" && !strings.HasPrefix(line, "SELECT ") {
			t.Fatal("schema check wrote data", line)
		}
	}
}

func TestTaskIdentityDataRejectsMissingValuesAndPropagatesFailure(t *testing.T) {
	f := gormfake.New()
	db := gormfake.MustOpen(f)
	if err := checkTaskIdentityData(db); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Written(), "identity_type IS NULL") {
		t.Fatal("NULL identity type escaped validation", f.Written())
	}
	if !strings.Contains(f.Written(), "LIMIT ?) AS invalid_identity") || !strings.HasSuffix(strings.TrimSpace(f.Written()), "| 1") {
		t.Fatal("identity rejection must stop at its first witness", f.Written())
	}
	f.SetCounts(0, 1)
	if err := checkTaskIdentityData(db); err == nil {
		t.Fatal("invalid identity row accepted")
	}
	assertSchemaReadOnly(t, f.Written())
	f.SetQueryError("crawler_tasks", "read unavailable")
	if err := checkTaskIdentityData(db); err == nil || !strings.Contains(err.Error(), "read unavailable") {
		t.Fatalf("lost read error: %v", err)
	}
}
