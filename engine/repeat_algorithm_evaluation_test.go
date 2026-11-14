package engine

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// Opt-in measurements compare the frozen aggregate with the production single
// statement in a disposable schema; no cache, index or business data changes.
func TestAlgorithmRepeatMySQL(t *testing.T) {
	if os.Getenv("PAPA_ALGORITHM_LOAD_TEST") != "1" {
		t.Skip("PAPA_ALGORITHM_LOAD_TEST=1 required for synthetic scale measurements")
	}
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	var version string
	if err := db.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Log("server_version", version)
	previous := 0
	for _, size := range []int{1000, 10000, 100000} {
		future := time.Now().Add(time.Hour)
		for start := previous; start < size; start += 500 {
			rows := make([]models.CrawlerTask, 0, 500)
			for i := start; i < min(start+500, size); i++ {
				rows = append(rows, models.CrawlerTask{Site: fmt.Sprintf("synthetic-%02d", i%16), Stage: "detail", URL: fmt.Sprintf("https://example.invalid/%d", i), Status: models.TaskStatus((i / 16) % 4), Repeatable: models.RepeatableYes, Repeat: (i / 64) % 10, NextRepeatAt: &future})
			}
			if err := db.CreateInBatches(rows, 500).Error; err != nil {
				t.Fatal(err)
			}
		}
		previous = size
		for _, distribution := range []string{"dense_future", "sparse_future", "mixed_due", "all_capped"} {
			if err := db.Exec("UPDATE crawler_tasks SET `repeat` = MOD(FLOOR((id - 1) / 64), 10), repeatable = 1, next_repeat_at = FROM_UNIXTIME(1794618000)").Error; err != nil {
				t.Fatal(err)
			}
			if distribution == "sparse_future" {
				if err := db.Exec("UPDATE crawler_tasks SET repeatable = IF(MOD(id, 100) = 0, 1, 0)").Error; err != nil {
					t.Fatal(err)
				}
			}
			if distribution == "mixed_due" {
				if err := db.Exec("UPDATE crawler_tasks SET next_repeat_at = NULL WHERE MOD(id, 10) = 0").Error; err != nil {
					t.Fatal(err)
				}
			}
			if distribution == "all_capped" {
				if err := db.Exec("UPDATE crawler_tasks SET `repeat` = 100").Error; err != nil {
					t.Fatal(err)
				}
			}
			// Refresh optimizer statistics after changing cardinality/distribution.
			if err := db.Exec("ANALYZE TABLE crawler_tasks").Error; err != nil {
				t.Fatal(err)
			}
			for _, limits := range []bool{false, true} {
				e.stages = make(map[string]*stageInfo)
				if limits {
					for i := 0; i < 16; i++ {
						site := fmt.Sprintf("synthetic-%02d", i)
						e.stages[site+":detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: site, RepeatLimit: 5}}
					}
				}
				t.Run(fmt.Sprintf("rows_%d/%s/limits_%t", size, distribution, limits), func(t *testing.T) {
					if err := db.Connection(func(tx *gorm.DB) error {
						// Connection hands out a reusable statement. Clone for each
						// query, otherwise SET/WHERE from one probe pollutes the next.
						tx = tx.Session(&gorm.Session{NewDB: true})
						// NOW is frozen only on this dedicated connection, making
						// the null=>now comparison exact across candidate queries.
						if err := tx.Exec("SET timestamp = 1794614400").Error; err != nil {
							return err
						}
						defer func() {
							if err := tx.Exec("SET timestamp = 0").Error; err != nil {
								t.Error(err)
							}
						}()
						e.db = tx
						defer func() { e.db = db }()
						var baseline struct{ Epoch *int64 }
						if err := e.withRepeatLimits(referenceSoonestRepeatQueryOn(tx)).Scan(&baseline).Error; err != nil {
							return err
						}
						if baseline.Epoch == nil && !(distribution == "all_capped" && limits) {
							t.Fatal("fixture must contain eligible repeat rows")
						}
						if distribution == "all_capped" && limits && baseline.Epoch != nil {
							t.Fatal("all-capped fixture still contains an eligible row")
						}
						wantEpoch := int64(1794618000)
						if distribution == "mixed_due" {
							wantEpoch = 1794614400
						}
						if baseline.Epoch != nil && *baseline.Epoch != wantEpoch {
							t.Fatalf("aggregate=%d expected=%d", *baseline.Epoch, wantEpoch)
						}
						candidate := func() (*int64, error) {
							var row struct{ Epoch *int64 }
							err := e.soonestRepeatQueryOn(tx).Scan(&row).Error
							return row.Epoch, err
						}
						got, err := candidate()
						if err != nil {
							return err
						}
						if (got == nil) != (baseline.Epoch == nil) || got != nil && *got != *baseline.Epoch {
							t.Fatal("candidate changed earliest due time")
						}
						var count int64
						if err := e.repeatQueueQuery()().Count(&count).Error; err != nil {
							return err
						}
						expected := int64(0)
						for i := 0; i < size; i++ {
							if distribution == "mixed_due" && (i+1)%10 == 0 && (i/16)%4 >= 2 && (!limits || (i/64)%10 < 5) {
								expected++
							}
						}
						if count != expected {
							t.Fatalf("due set=%d expected=%d", count, expected)
						}
						t.Logf("due_rows=%d", count)
						queries := []struct {
							name string
							run  func() error
							sql  string
						}{
							{"soonest", func() error {
								var r struct{ Epoch *int64 }
								return e.withRepeatLimits(referenceSoonestRepeatQueryOn(tx)).Scan(&r).Error
							}, tx.ToSQL(func(d *gorm.DB) *gorm.DB {
								var r struct{ Epoch *int64 }
								return e.withRepeatLimits(referenceSoonestRepeatQueryOn(d)).Scan(&r)
							})},
							{"optimized_single_statement", func() error { _, err := candidate(); return err }, tx.ToSQL(func(d *gorm.DB) *gorm.DB { var row struct{ Epoch *int64 }; return e.soonestRepeatQueryOn(d).Find(&row) })},
							{"due_first_page", func() error {
								var rows []models.CrawlerTask
								return e.repeatQueueQuery()().Order("id").Limit(100).Find(&rows).Error
							}, tx.ToSQL(func(d *gorm.DB) *gorm.DB {
								var rows []models.CrawlerTask
								return e.withRepeatLimits(repeatBaseQueryOn(d)).Where("next_repeat_at IS NULL OR next_repeat_at <= NOW()").Order("id").Limit(100).Find(&rows)
							})},
							{"due_count", func() error { var count int64; return e.repeatQueueQuery()().Count(&count).Error }, ""},
						}
						for _, q := range queries {
							if err := q.run(); err != nil {
								return err
							}
							var samples []float64
							for i := 0; i < 9; i++ {
								begin := time.Now()
								if err := q.run(); err != nil {
									return err
								}
								samples = append(samples, float64(time.Since(begin))/float64(time.Millisecond))
							}
							sort.Float64s(samples)
							before := algorithmHandlerReads(t, tx)
							if err := q.run(); err != nil {
								return err
							}
							after := algorithmHandlerReads(t, tx)
							if q.name == "soonest" && after <= before {
								t.Fatal("aggregate did not read fixture rows")
							}
							t.Logf("query=%s p50_ms=%.3f max_ms=%.3f handler_reads=%d", q.name, samples[4], samples[8], after-before)
							if q.sql != "" {
								var plan string
								if err := tx.Raw("EXPLAIN FORMAT=JSON " + q.sql).Scan(&plan).Error; err != nil {
									return err
								}
								t.Logf("plan=%s %s", q.name, plan)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func algorithmHandlerReads(t *testing.T, db *gorm.DB) uint64 {
	t.Helper()
	var rows []struct {
		VariableName string `gorm:"column:Variable_name"`
		Value        uint64
	}
	if err := db.Raw("SHOW SESSION STATUS WHERE Variable_name IN ('Handler_read_first','Handler_read_key','Handler_read_next','Handler_read_prev','Handler_read_rnd','Handler_read_rnd_next')").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("missing handler counters: %d", len(rows))
	}
	var total uint64
	for _, r := range rows {
		total += r.Value
	}
	return total
}

// Frozen pre-optimization expression: do not derive the reference from the
// new builder, or semantic regressions would silently change both sides.
func referenceSoonestRepeatQueryOn(db *gorm.DB) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("repeatable = ?", models.RepeatableYes).
		Select(`MIN(CAST(CASE WHEN status IN ? THEN UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) WHEN next_repeat_at > NOW() THEN UNIX_TIMESTAMP(next_repeat_at) ELSE NULL END AS SIGNED)) AS epoch`, []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed})
}
