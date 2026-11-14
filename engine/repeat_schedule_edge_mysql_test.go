package engine

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
)

// These values can exist even after a session returns to strict SQL mode.
// Test mixed rows: an invalid later DATETIME must not be hidden by a valid
// earlier row when MySQL maps the invalid date to epoch zero.
func TestRepeatScheduleMySQLInvalidDates(t *testing.T) {
	db, _ := compensationMySQL(t)
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
		var originalMode string
		if err := tx.Raw("SELECT @@session.sql_mode").Scan(&originalMode).Error; err != nil {
			return err
		}
		defer func() {
			if err := tx.Exec("SET SESSION sql_mode = ?", originalMode).Error; err != nil {
				t.Error(err)
			}
		}()
		for _, limit := range []int{-1, 0, 3} {
			e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: limit}}}}
			for _, status := range []any{0, 1, 2, 3, 9, nil} {
				for _, at := range []string{"2027-00-01 00:00:00", "2027-01-00 00:00:00", "2027-02-30 00:00:00", "2027-04-31 00:00:00", "0000-00-00 00:00:00"} {
					t.Run(fmt.Sprintf("limit_%d/status_%v/%s", limit, status, at), func(t *testing.T) {
						if err := tx.Exec("SET SESSION sql_mode = 'ALLOW_INVALID_DATES'").Error; err != nil {
							t.Fatal(err)
						}
						insertRepeatScheduleRows(t, tx, repeatScheduleRow{status: 2, round: 1, at: "2020-01-01 00:00:00"}, repeatScheduleRow{status: status, round: 1, at: at})
						// Reading existing values must not depend on the insert mode.
						if err := tx.Exec("SET SESSION sql_mode = 'STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE'").Error; err != nil {
							t.Fatal(err)
						}
						var stored string
						if err := tx.Raw("SELECT LEFT(CAST(next_repeat_at AS CHAR), 19) FROM crawler_tasks WHERE id = 2").Scan(&stored).Error; err != nil {
							t.Fatal(err)
						}
						if stored != at {
							t.Fatalf("date fixture was coerced: stored=%q want=%q", stored, at)
						}
						got, old := repeatScheduleEpoch(t, e, tx, false), repeatScheduleEpoch(t, e, tx, true)
						var validEpoch int64
						if err := tx.Raw("SELECT CAST(UNIX_TIMESTAMP('2020-01-01 00:00:00') AS SIGNED)").Scan(&validEpoch).Error; err != nil {
							t.Fatal(err)
						}
						want := int64(0)
						if at == "0000-00-00 00:00:00" && status != 2 && status != 3 {
							want = validEpoch
						}
						if got == nil || *got != want || !sameRepeatEpoch(got, old) {
							t.Fatalf("invalid date changed MIN(epoch): got=%v old=%v want=%d", got, old, want)
						}
					})
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatScheduleMySQLCallerScopeAndLimit(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := &Engine{}
	insertRepeatScheduleRows(t, db,
		repeatScheduleRow{status: 2, round: 1, site: "excluded", at: "2020-01-01 00:00:00"},
		repeatScheduleRow{status: 2, round: 1, site: "wanted", at: "2027-01-01 00:00:00"},
		repeatScheduleRow{status: 2, round: 1, site: "wanted", at: "2028-01-01 00:00:00"})
	for _, tc := range []struct {
		name  string
		apply func(*gorm.DB) *gorm.DB
		empty bool
	}{
		{"where", func(d *gorm.DB) *gorm.DB { return d.Where("site = ?", "wanted") }, false},
		{"deferred_scope", func(d *gorm.DB) *gorm.DB {
			return d.Scopes(func(q *gorm.DB) *gorm.DB { return q.Where("site = ?", "wanted") })
		}, false},
		{"aggregate_order", func(d *gorm.DB) *gorm.DB { return d.Where("site = ?", "wanted").Order("epoch DESC") }, false},
		{"limit_zero", func(d *gorm.DB) *gorm.DB { return d.Limit(0) }, true},
		{"offset", func(d *gorm.DB) *gorm.DB { return d.Limit(1).Offset(1) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := repeatScheduleEpoch(t, e, tc.apply(db), false)
			old := repeatScheduleEpoch(t, e, tc.apply(db), true)
			var want int64
			if err := db.Raw("SELECT CAST(UNIX_TIMESTAMP('2027-01-01 00:00:00') AS SIGNED)").Scan(&want).Error; err != nil {
				t.Fatal(err)
			}
			if !sameRepeatEpoch(got, old) || (got == nil) != tc.empty || got != nil && *got != want {
				t.Fatalf("caller clauses changed: got=%v old=%v empty=%v", got, old, tc.empty)
			}
		})
	}
}

func TestRepeatScheduleMySQLNullableCaps(t *testing.T) {
	db, _ := compensationMySQL(t)
	for _, limit := range []int{-1, 0, 3} {
		e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: limit}}}}
		for _, tc := range []struct {
			name               string
			site, stage, round any
			cappedEligible     bool
		}{
			{"below", "limited", "detail", 2, true},
			{"at", "limited", "detail", 3, false},
			{"above", "limited", "detail", 4, false},
			{"null_round", "limited", "detail", nil, false},
			{"other_site_null_round", "other", "detail", nil, true},
			{"other_stage_null_round", "limited", "other", nil, true},
			{"null_site_at_limit", nil, "detail", 3, false},
			{"empty_stage_at_limit", "limited", "", 3, true},
			{"null_site_below", nil, "detail", 2, true},
			{"other_stage_at_limit", "limited", "other", 3, true},
		} {
			t.Run(fmt.Sprintf("limit_%d/%s", limit, tc.name), func(t *testing.T) {
				insertRepeatScheduleRows(t, db, repeatScheduleRow{status: 2, round: 1, at: "2027-01-01 00:00:00"})
				if err := db.Exec("UPDATE crawler_tasks SET site = ?, stage = ?, `repeat` = ?", tc.site, tc.stage, tc.round).Error; err != nil {
					t.Fatal(err)
				}
				var actual struct {
					Site  *string
					Stage string
					Round *int64 `gorm:"column:repeat"`
				}
				if err := db.Raw("SELECT site, stage, `repeat` FROM crawler_tasks").Scan(&actual).Error; err != nil {
					t.Fatal(err)
				}
				if (actual.Site == nil) != (tc.site == nil) || actual.Site != nil && *actual.Site != tc.site || actual.Stage != tc.stage || (actual.Round == nil) != (tc.round == nil) || actual.Round != nil && int(*actual.Round) != tc.round {
					t.Fatal("fixture was coerced by MySQL")
				}
				got, old := repeatScheduleEpoch(t, e, db, false), repeatScheduleEpoch(t, e, db, true)
				eligible := limit <= 0 || tc.cappedEligible
				var want int64
				if err := db.Raw("SELECT CAST(UNIX_TIMESTAMP('2027-01-01 00:00:00') AS SIGNED)").Scan(&want).Error; err != nil {
					t.Fatal(err)
				}
				if !sameRepeatEpoch(got, old) || (got != nil) != eligible || got != nil && *got != want {
					t.Fatalf("NULL/limit predicate changed: got=%v old=%v eligible=%v", got, old, eligible)
				}
			})
		}
	}
}
