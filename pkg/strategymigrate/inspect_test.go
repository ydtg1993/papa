package strategymigrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	mysql "github.com/go-sql-driver/mysql"
	"io"
	"strings"
	"testing"
)

type inventoryConn struct {
	readOnly, rolledBack bool
	queries              int
	failCount            bool
	deniedMissing        bool
}
type connector struct{ c *inventoryConn }

func (x connector) Connect(context.Context) (driver.Conn, error) { return x.c, nil }
func (x connector) Driver() driver.Driver                        { return inventoryDriver{x.c} }

type inventoryDriver struct{ c *inventoryConn }

func (x inventoryDriver) Open(string) (driver.Conn, error) { return x.c, nil }
func (c *inventoryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *inventoryConn) Close() error { return nil }
func (c *inventoryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("read-only transaction required")
}
func (c *inventoryConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	c.readOnly = o.ReadOnly
	return c, nil
}
func (c *inventoryConn) Commit() error   { return errors.New("inspection must not commit") }
func (c *inventoryConn) Rollback() error { c.rolledBack = true; return nil }

type oneRow struct {
	n    int64
	done bool
}

func (r *oneRow) Columns() []string { return []string{"count"} }
func (r *oneRow) Close() error      { return nil }
func (r *oneRow) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	v[0] = r.n
	r.done = true
	return nil
}
func (c *inventoryConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries++
	if !c.readOnly || !strings.HasPrefix(q, "SELECT ") {
		return nil, errors.New("write or unguarded read")
	}
	if strings.Contains(q, "`crawler_strategy_") {
		if c.deniedMissing {
			return nil, &mysql.MySQLError{Number: 1142, Message: "denied"}
		}
		return nil, &mysql.MySQLError{Number: 1146, Message: "missing"}
	}
	if c.failCount {
		return nil, errors.New("secret driver diagnostics")
	}
	return &oneRow{n: 7}, nil
}
func TestInspectionIsReadOnlyAndMissingIsNotEmpty(t *testing.T) {
	c := &inventoryConn{}
	db := sql.OpenDB(connector{c})
	defer db.Close()
	r, e := Inspect(context.Background(), db, []TableSpec{{"example_sessions", "retire_after_verified_migration"}})
	if e != nil || !r.ReadOnly || r.MigrationReady || !c.readOnly || !c.rolledBack {
		t.Fatal(r, e)
	}
	missing := 0
	for _, v := range r.Tables {
		if !v.Exists {
			missing++
			if v.Rows != nil {
				t.Fatal("missing reported as zero")
			}
		} else if v.Rows == nil || *v.Rows != 7 {
			t.Fatal("wrong row count")
		}
	}
	if missing != 5 || len(r.Tables) != 21 {
		t.Fatal("incomplete inventory", missing, len(r.Tables))
	}
}
func TestInspectionRejectsUnsafeManifestBeforeQuery(t *testing.T) {
	for _, extra := range [][]TableSpec{
		{{"example;DROP TABLE x", "retain"}}, {{"crawler_tasks", "retain"}}, {{"example_data", "drop_now"}},
	} {
		c := &inventoryConn{}
		db := sql.OpenDB(connector{c})
		_, e := Inspect(context.Background(), db, extra)
		db.Close()
		if e == nil || c.queries != 0 {
			t.Fatal("unsafe inventory accepted", extra)
		}
	}
}
func TestInspectionFailureCannotLookLikeCompleteReport(t *testing.T) {
	c := &inventoryConn{failCount: true}
	db := sql.OpenDB(connector{c})
	defer db.Close()
	r, e := Inspect(context.Background(), db, nil)
	if e == nil || strings.Contains(e.Error(), "secret") || r.MigrationReady || !c.rolledBack {
		t.Fatal(r, e)
	}
}

func TestInspectionPermissionDeniedIsNotMissingTable(t *testing.T) {
	c := &inventoryConn{deniedMissing: true}
	db := sql.OpenDB(connector{c})
	defer db.Close()
	r, e := Inspect(context.Background(), db, nil)
	if e == nil || r.MigrationReady {
		t.Fatal("permission denial treated as absent", r, e)
	}
}
