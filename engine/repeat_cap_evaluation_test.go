package engine

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// Explicitly opt in: fresh synthetic schema, no runtime strategy or cache.
// Rotate query order each round instead of measuring one version entirely
// before the next. Timings are observations, never environment-specific gates.
func TestRepeatCapCostMySQL(t *testing.T) {
	if os.Getenv("PAPA_ALGORITHM_LOAD_TEST") != "1" {
		t.Skip("PAPA_ALGORITHM_LOAD_TEST=1 required for synthetic cap-cost measurements")
	}
	db, _ := compensationMySQL(t)
	var version string
	if err := db.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Log("server_version", version)
	previous := 0
	for _, size := range []int{1000, 100000} {
		future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		for start := previous; start < size; start += 500 {
			rows := make([]models.CrawlerTask, 0, 500)
			for i := start; i < min(start+500, size); i++ {
				rows = append(rows, models.CrawlerTask{Site: fmt.Sprintf("synthetic-%02d", i%16), Stage: "detail", URL: fmt.Sprintf("https://example.invalid/cap/%d", i), Status: models.TaskStatus((i / 16) % 4), Repeatable: models.RepeatableYes, Repeat: 100, NextRepeatAt: &future})
			}
			if err := db.CreateInBatches(rows, 500).Error; err != nil {
				t.Fatal(err)
			}
		}
		previous = size
		for _, distribution := range []string{"all_capped", "dense_future", "expired_active"} {
			if err := db.Exec("UPDATE crawler_tasks SET status = MOD(FLOOR((id - 1) / 16), 4), `repeat` = MOD(FLOOR((id - 1) / 64), 10), next_repeat_at = FROM_UNIXTIME(1794618000)").Error; err != nil {
				t.Fatal(err)
			}
			if distribution == "all_capped" {
				if err := db.Exec("UPDATE crawler_tasks SET `repeat` = 100").Error; err != nil {
					t.Fatal(err)
				}
			}
			if distribution == "expired_active" {
				if err := db.Exec("UPDATE crawler_tasks SET status = IF(MOD(id, 100) = 0, 2, 0), next_repeat_at = FROM_UNIXTIME(1577836800)").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Exec("ANALYZE TABLE crawler_tasks").Error; err != nil {
				t.Fatal(err)
			}
			e := &Engine{stages: make(map[string]*stageInfo)}
			for i := 0; i < 16; i++ {
				site := fmt.Sprintf("synthetic-%02d", i)
				e.stages[site+":detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: site, RepeatLimit: 5}}
			}
			t.Run(fmt.Sprintf("rows_%d/%s", size, distribution), func(t *testing.T) {
				if err := db.Connection(func(conn *gorm.DB) error {
					tx := conn.Session(&gorm.Session{NewDB: true})
					if err := tx.Exec("SET timestamp = 1794614400").Error; err != nil {
						return err
					}
					defer func() {
						if err := tx.Exec("SET timestamp = 0").Error; err != nil {
							t.Error(err)
						}
					}()
					var fixture struct{ Total, Repeating, UnderCap int64 }
					if err := tx.Raw("SELECT COUNT(*) AS total, SUM(repeatable = 1) AS repeating, SUM(`repeat` < 5) AS under_cap FROM crawler_tasks").Scan(&fixture).Error; err != nil {
						return err
					}
					var wantUnderCap int64
					if distribution != "all_capped" {
						for i := 0; i < size; i++ {
							if (i/64)%10 < 5 {
								wantUnderCap++
							}
						}
					}
					if fixture.Total != int64(size) || fixture.Repeating != int64(size) || fixture.UnderCap != wantUnderCap {
						return fmt.Errorf("fixture cardinality=%+v want total/repeating=%d under_cap=%d", fixture, size, wantUnderCap)
					}
					variants := []struct {
						name  string
						query func(*gorm.DB) *gorm.DB
					}{
						{"original_case", func(d *gorm.DB) *gorm.DB { return referenceRepeatLimits(e, referenceSoonestRepeatQueryOn(d)) }},
						{"status_first", func(d *gorm.DB) *gorm.DB {
							return referenceRepeatScheduleStatusFirst(e, d)
						}},
						{"caps_first", func(d *gorm.DB) *gorm.DB {
							return referenceRepeatLimits(e, d.Model(&models.CrawlerTask{}).Where("repeatable = ?", models.RepeatableYes)).Where("status IN ? OR next_repeat_at > NOW()", repeatDueStatuses).Select("MIN(CAST(UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) AS SIGNED)) AS epoch")
						}},
						// A statement-local diagnostic, not a server/session setting
						// or production hint. EXPLAIN must confirm its actual effect.
						{"no_icp", func(d *gorm.DB) *gorm.DB {
							return referenceRepeatScheduleStatusFirst(e, d).Select("/*+ NO_ICP(crawler_tasks) */ MIN(CAST(UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) AS SIGNED)) AS epoch")
						}},
						{"grouped_caps", e.soonestRepeatQueryOn},
					}
					want := int64(1794618000)
					if distribution == "expired_active" {
						want = 1577836800
					}
					run := func(i int) error {
						var row struct{ Epoch *int64 }
						if err := variants[i].query(tx).Scan(&row).Error; err != nil {
							return err
						}
						if distribution == "all_capped" {
							if row.Epoch != nil {
								return fmt.Errorf("%s did not exclude all rows", variants[i].name)
							}
						} else if row.Epoch == nil || *row.Epoch != want {
							return fmt.Errorf("%s lost independent expected epoch %d", variants[i].name, want)
						}
						return nil
					}
					for i := range variants {
						if err := run(i); err != nil {
							return err
						}
					}
					samples := make([][]float64, len(variants))
					for round := 0; round < 15; round++ {
						for offset := range variants {
							i := (round + offset) % len(variants)
							start := time.Now()
							if err := run(i); err != nil {
								return err
							}
							samples[i] = append(samples[i], float64(time.Since(start))/float64(time.Millisecond))
						}
					}
					for i, v := range variants {
						sort.Float64s(samples[i])
						before := algorithmHandlerReads(t, tx)
						if err := run(i); err != nil {
							return err
						}
						reads := algorithmHandlerReads(t, tx) - before
						if reads == 0 {
							return fmt.Errorf("%s did not read fixture", v.name)
						}
						t.Logf("query=%s p50_ms=%.3f max_ms=%.3f handler_reads=%d", v.name, samples[i][len(samples[i])/2], samples[i][len(samples[i])-1], reads)
						sql := tx.ToSQL(func(d *gorm.DB) *gorm.DB { var row struct{ Epoch *int64 }; return v.query(d).Find(&row) })
						t.Logf("sql=%s %s", v.name, sql)
						for _, explain := range []string{"EXPLAIN FORMAT=JSON ", "EXPLAIN ANALYZE "} {
							var plan string
							if err := tx.Raw(explain + sql).Scan(&plan).Error; err != nil {
								return err
							}
							var warnings []struct {
								Level, Message string
								Code           int
							}
							if err := tx.Raw("SHOW WARNINGS").Scan(&warnings).Error; err != nil {
								return err
							}
							for _, warning := range warnings {
								if warning.Level == "Warning" || warning.Level == "Error" {
									return fmt.Errorf("%s explain warning: %+v", v.name, warning)
								}
							}
							if v.name == "no_icp" && strings.Contains(plan, "with index condition:") {
								return fmt.Errorf("NO_ICP diagnostic was ignored")
							}
							t.Logf("plan=%s %s\n%s", v.name, explain, plan)
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

// The previous production shape, independent of the current cap folding.
func referenceRepeatScheduleStatusFirst(e *Engine, d *gorm.DB) *gorm.DB {
	return referenceRepeatLimits(e, d.Model(&models.CrawlerTask{}).Where("repeatable = ?", models.RepeatableYes).
		Where("status IN ? OR next_repeat_at > NOW()", repeatDueStatuses).
		Select("MIN(CAST(UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) AS SIGNED)) AS epoch"))
}
