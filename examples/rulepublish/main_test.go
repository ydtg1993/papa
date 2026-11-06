package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestRulePublicationExampleMySQL(t *testing.T) {
	dsn := os.Getenv("PAPA_RULE_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_RULE_EXAMPLE_TEST_DSN to a dedicated disposable MySQL database")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "rule_test") {
		t.Fatal("example integration test requires a dedicated rule_test database")
	}
	t.Setenv("PAPA_RULE_DSN", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := command(ctx, &out, t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	var result demoResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.VerifiedVersions != 4 || result.OldTaskTitle != "Alpha" || result.NewTaskTitle != "Beta" || result.RollbackTitle != "Alpha" || !result.ChildPinned || !result.SampleRestored {
		t.Fatalf("unexpected publication demo: %+v", result)
	}
}
