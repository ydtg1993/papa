package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This callback supplies ordered pages; it is not a SQL execution oracle.
// Assertions on the MySQL DryRun statement separately pin the production SQL shape.
func TestDedupWarmupPagesAndCapacitySQL(t *testing.T) {
	for _, capacity := range []int{2, 300, 0} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			db := dryRunMySQL(t)
			const total = 600
			calls, scanned := 0, 0
			err := db.Callback().Query().After("gorm:query").Register("test:warmup", func(tx *gorm.DB) {
				calls++
				if calls > total {
					t.Fatal("warmup did not advance")
				}
				limit, ok := tx.Statement.Clauses["LIMIT"].Expression.(clause.Limit)
				if !ok || limit.Limit == nil || *limit.Limit <= 0 || *limit.Limit > 256 {
					t.Fatalf("warmup query must have bounded LIMIT <= 256: %s", tx.Statement.SQL.String())
				}
				sql := tx.Statement.SQL.String()
				order := "ORDER BY id DESC"
				if capacity > 0 {
					order = "ORDER BY updated_at DESC,id DESC"
				}
				for _, want := range []string{"`id`", "`url`", "`stage`", "`site`", "`idempotency_key`", "status IN", "repeatable =", order, "LIMIT ?"} {
					if !strings.Contains(sql, want) {
						t.Fatalf("SQL missing %q: %s", want, sql)
					}
				}
				if strings.Contains(sql, "SELECT *") {
					t.Fatalf("wide projection: %s", sql)
				}
				if capacity > 0 && limit.Offset != scanned {
					t.Fatalf("offset=%d scanned=%d", limit.Offset, scanned)
				}
				if capacity == 0 {
					if limit.Offset != 0 {
						t.Fatalf("unlimited mode must use a cursor, got offset=%d", limit.Offset)
					}
					if scanned > 0 {
						if !strings.Contains(sql, "WHERE (status IN (?,?) OR repeatable = ?) AND id < ?") {
							t.Fatalf("cursor predicate must apply to both active categories: %s", sql)
						}
						if tx.Statement.Vars[3] != uint(total-scanned+1) {
							t.Fatalf("cursor vars=%v", tx.Statement.Vars)
						}
					}
				}
				if capacity > 0 && scanned+*limit.Limit > capacity {
					t.Fatalf("query exceeds cache capacity: %s", sql)
				}
				end := min(total, scanned+*limit.Limit)
				rows := make([]models.CrawlerTask, 0, end-scanned)
				for i := scanned; i < end; i++ {
					rows = append(rows, models.CrawlerTask{ID: uint(total - i), URL: fmt.Sprintf("https://example.test/%d", i), Site: "s", Stage: "detail"})
				}
				*tx.Statement.Dest.(*[]models.CrawlerTask) = rows
				tx.RowsAffected = int64(len(rows))
				scanned = end
			})
			if err != nil {
				t.Fatal(err)
			}
			e := &Engine{db: db, loggerSet: &loggers.LoggerSet{DB: quietLogger()}, dedupCache: newDedupCache(capacity)}
			e.loadActiveTasks()
			want := total
			if capacity > 0 {
				want = capacity
			}
			if scanned != want || e.dedupCache.Len() != want {
				t.Fatalf("scanned=%d cached=%d want=%d", scanned, e.dedupCache.Len(), want)
			}
			for i := 0; i < want; i++ {
				if _, ok := e.dedupCache.entries[fmt.Sprintf("s:detail:https://example.test/%d", i)]; !ok {
					t.Fatalf("missing recent row %d", i)
				}
			}
			// The most recently active page must stay newest in the initial LRU order.
			if got := e.dedupCache.lru.Front().Value.(*dedupEntry).key; got != "s:detail:https://example.test/0" {
				t.Fatalf("newest row not at LRU front: %s", got)
			}
			if capacity != 2 && calls < 2 {
				t.Fatalf("expected multiple bounded pages, got %d", calls)
			}
		})
	}
}
