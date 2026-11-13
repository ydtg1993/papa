package configstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This driver blocks inside the actual database/sql QueryContext call. It proves
// that GORM and the TTL adapter propagate cancellation, not only a fake port.
type contextQueryConnector struct {
	entered chan contextQueryCall
	release chan struct{}
}

type contextQueryCall struct {
	ctx   context.Context
	query string
}

func (c *contextQueryConnector) Connect(context.Context) (driver.Conn, error) {
	return &contextQueryConn{c}, nil
}
func (*contextQueryConnector) Driver() driver.Driver { return contextQueryDriver{} }

type contextQueryDriver struct{}

func (contextQueryDriver) Open(string) (driver.Conn, error) { return nil, driver.ErrSkip }

type contextQueryConn struct{ connector *contextQueryConnector }

func (*contextQueryConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*contextQueryConn) Close() error                        { return nil }
func (*contextQueryConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *contextQueryConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.connector.entered <- contextQueryCall{ctx, query}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.connector.release:
		return nil, errors.New("test cleanup released query")
	}
}

func TestStoreAndCachedStringListReaderPassContextToSQL(t *testing.T) {
	for _, method := range []string{"raw", "strings", "cached legacy constructor", "cached context constructor"} {
		t.Run(method, func(t *testing.T) {
			connector := &contextQueryConnector{entered: make(chan contextQueryCall, 1), release: make(chan struct{})}
			sqlDB := sql.OpenDB(connector)
			t.Cleanup(func() { close(connector.release); _ = sqlDB.Close() })
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
				DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			store := New(db)
			type marker struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), marker{}, "caller context"))
			defer cancel()
			result := make(chan error, 1)
			go func() {
				var err error
				switch method {
				case "raw":
					_, _, err = store.LoadContext(ctx, KeyIPWhitelist)
				case "strings":
					_, err = store.LoadStringsContext(ctx, KeyIPWhitelist)
				case "cached legacy constructor":
					_, err = NewCachedStringListReader(store, time.Minute).LoadStringsContext(ctx, KeyIPWhitelist)
				case "cached context constructor":
					_, err = NewCachedContextStringListReader(store, time.Minute).LoadStringsContext(ctx, KeyIPWhitelist)
				}
				result <- err
			}()
			select {
			case call := <-connector.entered:
				if call.ctx.Value(marker{}) != "caller context" || !strings.Contains(call.query, "crawler_config") {
					t.Fatalf("SQL did not receive caller context: marker=%v query=%q", call.ctx.Value(marker{}), call.query)
				}
			case <-time.After(time.Second):
				t.Fatal("query did not reach SQL driver")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("query cancellation=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("SQL query ignored caller cancellation")
			}
		})
	}
}
