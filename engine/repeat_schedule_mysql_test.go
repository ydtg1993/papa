package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

type repeatScheduleRow struct {
	status, at, round any
	site, stage, mode string
	disabled          bool
}

func insertRepeatScheduleRows(t *testing.T, db *gorm.DB, rows ...repeatScheduleRow) {
	t.Helper()
	if err := db.Exec("DELETE FROM crawler_tasks").Error; err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if r.site == "" {
			r.site = "limited"
		}
		if r.stage == "" {
			r.stage = "detail"
		}
		repeatable := 1
		if r.disabled {
			repeatable = 0
		}
		if err := db.Exec("INSERT INTO crawler_tasks (id, site, stage, identity_hash, url, status, repeatable, `repeat`, next_repeat_at, compensation_mode) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", i+1, r.site, r.stage, fmt.Sprintf("%064x", i+1), "https://example.invalid/fixture", r.status, repeatable, r.round, r.at, r.mode).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func repeatScheduleEpoch(t *testing.T, e *Engine, db *gorm.DB, legacy bool) *int64 {
	t.Helper()
	var row struct{ Epoch *int64 }
	q := e.soonestRepeatQueryOn(db)
	if legacy {
		q = e.withRepeatLimits(referenceSoonestRepeatQueryOn(db))
	}
	if err := q.Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Epoch
}

func sameRepeatEpoch(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func TestRepeatScheduleMySQLBoundaries(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: 3}}}}
	for _, zone := range []string{"SYSTEM", "+00:00", "+08:00"} {
		t.Run(zone, func(t *testing.T) {
			if err := db.Connection(func(conn *gorm.DB) error {
				tx := conn.Session(&gorm.Session{NewDB: true})
				if err := tx.Exec("SET time_zone = ?", zone).Error; err != nil {
					return err
				}
				defer func() {
					if err := tx.Exec("SET time_zone = 'SYSTEM'").Error; err != nil {
						t.Error(err)
					}
				}()
				if err := tx.Exec("SET timestamp = 1794614400").Error; err != nil {
					return err
				}
				defer func() {
					if err := tx.Exec("SET timestamp = 0").Error; err != nil {
						t.Error(err)
					}
				}()
				check := func(t *testing.T, wantAt any, eligible bool, rows ...repeatScheduleRow) {
					insertRepeatScheduleRows(t, tx, rows...)
					got, old := repeatScheduleEpoch(t, e, tx, false), repeatScheduleEpoch(t, e, tx, true)
					if !sameRepeatEpoch(got, old) {
						t.Fatalf("different epoch new=%v old=%v", got, old)
					}
					if !eligible {
						if got != nil {
							t.Fatalf("ineligible fixture got %d", *got)
						}
						return
					}
					var want int64
					q := "SELECT CAST(UNIX_TIMESTAMP(?) AS SIGNED)"
					args := []any{wantAt}
					if wantAt == nil {
						q = "SELECT CAST(UNIX_TIMESTAMP(NOW()) AS SIGNED)"
						args = nil
					}
					if err := tx.Raw(q, args...).Scan(&want).Error; err != nil {
						t.Fatal(err)
					}
					if got == nil || *got != want {
						t.Fatalf("epoch=%v want=%d", got, want)
					}
				}
				t.Run("empty", func(t *testing.T) { check(t, nil, false) })
				for _, status := range []any{0, 1, 2, 3, -7, 9, nil} {
					for _, at := range []any{nil, "2020-01-02 03:04:05.600", "2027-01-02 03:04:05.600"} {
						t.Run(fmt.Sprintf("status_%v/at_%v", status, at), func(t *testing.T) {
							terminal := status == 2 || status == 3
							eligible := terminal || at == "2027-01-02 03:04:05.600"
							check(t, at, eligible, repeatScheduleRow{status: status, at: at, round: 1})
						})
					}
				}
				for _, delta := range []int{-1000, 0, 1000} {
					var at string
					if err := tx.Raw("SELECT DATE_FORMAT(DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), '%Y-%m-%d %H:%i:%s.%f')", delta).Scan(&at).Error; err != nil {
						return err
					}
					for _, status := range []any{0, 1, 2, 3, 9, nil} {
						t.Run(fmt.Sprintf("now_%d/status_%v", delta, status), func(t *testing.T) {
							check(t, at, status == 2 || status == 3 || delta > 0, repeatScheduleRow{status: status, at: at, round: 1})
						})
					}
				}
				for _, at := range []string{"1969-01-01 00:00:00", "1970-12-31 23:59:59", "1971-01-01 00:00:00", "2037-12-31 23:59:59.999", "2038-01-01 00:00:00", "3002-01-01 00:00:00", "9999-12-31 23:59:59"} {
					t.Run(at, func(t *testing.T) { check(t, at, true, repeatScheduleRow{status: 2, at: at, round: 1}) })
				}
				t.Run("null_does_not_hide_past", func(t *testing.T) {
					check(t, "2020-01-01 00:00:00", true, repeatScheduleRow{status: 2, round: 1}, repeatScheduleRow{status: 2, round: 1, at: "2020-01-01 00:00:00"})
				})
				t.Run("out_of_range_future_zero_wins", func(t *testing.T) {
					check(t, "9999-12-31 23:59:59", true, repeatScheduleRow{status: 2, round: 1, at: "2027-01-01 00:00:00"}, repeatScheduleRow{status: 0, round: 1, at: "9999-12-31 23:59:59"})
				})
				for _, round := range []any{2, 3, 4, nil} {
					t.Run(fmt.Sprintf("limit_%v", round), func(t *testing.T) { check(t, nil, round == 2, repeatScheduleRow{status: 2, round: round}) })
				}
				t.Run("cap_before_limit", func(t *testing.T) {
					check(t, "2027-01-01 00:00:00", true, repeatScheduleRow{status: 2, round: 3, at: "2020-01-01 00:00:00"}, repeatScheduleRow{status: 2, round: 2, at: "2027-01-01 00:00:00"})
				})
				for _, r := range []repeatScheduleRow{{status: 2, round: 99, site: "other"}, {status: 2, round: 99, stage: "other"}, {status: 2, round: 1, mode: "manual"}} {
					t.Run(fmt.Sprintf("scope_%s_%s_%s", r.site, r.stage, r.mode), func(t *testing.T) { check(t, nil, true, r) })
				}
				t.Run("disabled", func(t *testing.T) { check(t, nil, false, repeatScheduleRow{status: 2, round: 1, disabled: true}) })
				t.Run("caller_scope", func(t *testing.T) {
					insertRepeatScheduleRows(t, tx, repeatScheduleRow{status: 2, round: 1, site: "excluded", at: "2020-01-01 00:00:00"}, repeatScheduleRow{status: 2, round: 1, site: "wanted", at: "2027-01-01 00:00:00"})
					scoped := tx.Where("site = ?", "wanted")
					got, old := repeatScheduleEpoch(t, e, scoped, false), repeatScheduleEpoch(t, e, scoped, true)
					if !sameRepeatEpoch(got, old) {
						t.Fatal("caller scope was discarded or applied after LIMIT")
					}
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepeatScheduleMySQLConcurrentSnapshot(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: 3}}}}
	insertRepeatScheduleRows(t, db, repeatScheduleRow{status: 2, round: 1, at: "2027-01-01 00:00:00"}, repeatScheduleRow{status: 3, round: 1, at: "2028-01-01 00:00:00"})
	want := repeatScheduleEpoch(t, e, db, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var writes atomic.Int64
	go func() {
		for ctx.Err() == nil {
			if err := db.Transaction(func(tx *gorm.DB) error {
				a, b := "2027-01-01 00:00:00", "2028-01-01 00:00:00"
				if writes.Load()%2 == 0 {
					a, b = b, a
				}
				return tx.Exec("UPDATE crawler_tasks SET next_repeat_at = CASE id WHEN 1 THEN ? ELSE ? END", a, b).Error
			}); err != nil {
				done <- err
				return
			}
			writes.Add(1)
		}
		done <- nil
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 300; i++ {
		got := repeatScheduleEpoch(t, e, db, false)
		if !sameRepeatEpoch(got, want) {
			t.Fatalf("torn snapshot on read %d", i)
		}
	}
	if writes.Load() < 10 {
		t.Fatalf("writer did not overlap enough: %d", writes.Load())
	}
	t.Logf("consistent_reads=300 committed_flips=%d", writes.Load())
}

func TestRepeatScheduleMySQLIsolation(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: 3}}}}
	for _, level := range []sql.IsolationLevel{sql.LevelReadCommitted, sql.LevelRepeatableRead} {
		t.Run(level.String(), func(t *testing.T) {
			insertRepeatScheduleRows(t, db, repeatScheduleRow{status: 2, round: 1, at: "2027-01-01 00:00:00"})
			if err := db.Transaction(func(tx *gorm.DB) error {
				before := repeatScheduleEpoch(t, e, tx, false)
				if err := db.Exec("UPDATE crawler_tasks SET next_repeat_at = '2028-01-01 00:00:00'").Error; err != nil {
					return err
				}
				after := repeatScheduleEpoch(t, e, tx, false)
				if level == sql.LevelRepeatableRead && !sameRepeatEpoch(before, after) {
					t.Fatal("RR view changed")
				}
				if level == sql.LevelReadCommitted && (after == nil || before == nil || *after <= *before) {
					t.Fatal("RC did not see committed change")
				}
				return nil
			}, &sql.TxOptions{Isolation: level}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepeatScheduleMySQLCancelBlockedQuery(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	// A metadata lock blocks the real UNION SELECT in MySQL. No injected sleep
	// or callback is used as proof that the query was actually in flight.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(context.Background(), "LOCK TABLES crawler_tasks WRITE"); err != nil {
		t.Fatal(err)
	}
	defer lock.ExecContext(context.Background(), "UNLOCK TABLES")
	var name string
	if err := db.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	e.cfg.RepeatQueue.Enabled = true
	e.startRepeatQueue()
	defer func() {
		e.cancel()
		// Failure must release the server lock before joining the blocked query.
		_, _ = lock.ExecContext(context.Background(), "UNLOCK TABLES")
		e.tickerWG.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM information_schema.processlist WHERE DB = ? AND STATE = 'Waiting for table metadata lock' AND INFO LIKE '%repeat_candidates%'", name).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("repeat SELECT never reached server lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if drained, _ := e.Stop(500 * time.Millisecond); drained {
		t.Fatal("Stop must not report drained while durable backlog count is blocked")
	}
	exited := make(chan struct{})
	go func() { e.tickerWG.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("query/ticker ignored cancellation")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("unbounded query cancellation")
	}
	// Release lock before Stop's separate durable-backlog accounting.
	if _, err := lock.ExecContext(context.Background(), "UNLOCK TABLES"); err != nil {
		t.Fatal(err)
	}
	if drained, stats := e.Stop(3 * time.Second); !drained || len(stats) != 0 {
		t.Fatalf("not drained: %v %v", drained, stats)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM information_schema.processlist WHERE DB = ? AND ID <> CONNECTION_ID() AND INFO LIKE '%repeat_candidates%'", name).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server repeat SELECT remained after unlock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := e.soonestRepeatIn(); ok {
		t.Fatal("canceled engine accepted new scheduling result")
	}
}
