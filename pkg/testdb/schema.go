// Package testdb owns disposable databases for explicit local acceptance tests.
// It is not used by the collection runtime.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sync"
	"time"
)

type Executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var testName = regexp.MustCompile(`(^|_)test(_|$)`)

// Create never adopts an existing database. Only a successful, exclusive CREATE
// grants ownership to the returned cleanup. Credentials/errors from the driver
// are deliberately not embedded in diagnostics.
func Create(ctx context.Context, db Executor, name string) (func() error, error) {
	if !identifier.MatchString(name) || !testName.MatchString(name) {
		return nil, fmt.Errorf("not a permitted temporary test database")
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4"); err != nil {
		return nil, fmt.Errorf("cannot exclusively create temporary database %s; existing databases are not reused or deleted", name)
	}
	var mu sync.Mutex
	done := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return nil
		}
		// Test timeout/cancellation must not suppress its cleanup.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := db.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			return fmt.Errorf("temporary database cleanup failed: %s", name)
		}
		done = true
		return nil
	}, nil
}
