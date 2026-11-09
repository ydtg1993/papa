package testdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type databaseStub struct {
	queries                []string
	createFails, dropFails bool
}

func (d *databaseStub) ExecContext(ctx context.Context, q string, _ ...any) (sql.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.queries = append(d.queries, q)
	if strings.HasPrefix(q, "CREATE") && d.createFails || strings.HasPrefix(q, "DROP") && d.dropFails {
		return nil, errors.New("driver failure with secret")
	}
	return nil, nil
}
func TestDisposableDatabaseRequiresExclusiveOwnership(t *testing.T) {
	for _, name := range []string{"production", "contest", "mysql", "test`", "fixture_test_012345abcdef`"} {
		d := &databaseStub{}
		if cleanup, err := Create(context.Background(), d, name); err == nil || cleanup != nil || len(d.queries) != 0 {
			t.Fatal("unsafe name accepted", name)
		}
	}
	d := &databaseStub{createFails: true}
	cleanup, err := Create(context.Background(), d, "fixture_test_012345abcdef")
	if err == nil || cleanup != nil || strings.Contains(err.Error(), "secret") || len(d.queries) != 1 || strings.Contains(d.queries[0], "IF NOT EXISTS") {
		t.Fatal("adopted prior database", err)
	}
}
func TestDisposableDatabaseCleanupSurvivesCancellationAndRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &databaseStub{}
	cleanup, err := Create(ctx, d, "fixture_test_012345abcdef")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	d.dropFails = true
	if err = cleanup(); err == nil {
		t.Fatal("drop failure swallowed")
	}
	d.dropFails = false
	if err = cleanup(); err != nil {
		t.Fatal(err)
	}
	if err = cleanup(); err != nil || len(d.queries) != 3 {
		t.Fatal("successful cleanup repeated", err)
	}
}
