package engine

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// Comparing WHERE result sets alone cannot distinguish FALSE from UNKNOWN.
// Project the actual production predicate as a nullable scalar as well.
func TestRepeatLimitMySQLThreeValuedEquivalence(t *testing.T) {
	db, _ := compensationMySQL(t)
	rows := make([]map[string]any, 0)
	for _, site := range []any{nil, "a", "b", "c", "d", "missing", "A", "á", "a ", "quote's"} {
		for _, stage := range []string{"detail", "catalog", "missing", "DETAIL", ""} {
			for _, round := range []any{nil, -1, 0, 1, 2, 3, 4, 5, 6} {
				id := len(rows) + 1
				rows = append(rows, map[string]any{"id": id, "site": site, "stage": stage, "repeat": round, "identity_hash": fmt.Sprintf("%064x", id), "url": "https://example.invalid/three-valued"})
			}
		}
	}
	if err := db.Table("crawler_tasks").CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"unlimited", "one_group", "mixed"} {
		t.Run(profile, func(t *testing.T) {
			e := &Engine{stages: make(map[string]*stageInfo)}
			add := func(site, stage string, limit int) {
				e.stages[site+":"+stage] = &stageInfo{stage: stage, config: StageConfig{Site: site, RepeatLimit: limit}}
			}
			add("free", "detail", 0)
			add("negative", "detail", -1)
			if profile != "unlimited" {
				add("a", "detail", 3)
				add("b", "detail", 3)
				add("quote's", "detail", 3)
			}
			if profile == "mixed" {
				add("c", "detail", 5)
				// Equal limits across different stages must stay separate too.
				add("a", "catalog", 3)
				add("d", "catalog", 3)
				// Depending on the real column collation, these can overlap
				// existing constraints. Folding must not change that behavior.
				add("A", "detail", 4)
				add("b", "DETAIL", 6)
			}
			predicate := func(folded bool) string {
				sql := db.ToSQL(func(d *gorm.DB) *gorm.DB {
					q := d.Table("crawler_tasks").Select("1")
					if folded {
						q = e.withRepeatLimits(q)
					} else {
						q = referenceRepeatLimits(e, q)
					}
					return q.Find(&[]struct{ Value int }{})
				})
				_, where, found := strings.Cut(sql, " WHERE ")
				if !found {
					return "1"
				}
				return where
			}
			var values []struct {
				ID               uint
				Folded, Original *int64
			}
			// Predicates come from GORM-rendered, constant synthetic declarations;
			// no external input is concatenated into this test SELECT.
			if err := db.Raw("SELECT id, (" + predicate(true) + ") AS folded, (" + predicate(false) + ") AS original FROM crawler_tasks ORDER BY id").Scan(&values).Error; err != nil {
				t.Fatal(err)
			}
			if len(values) != len(rows) {
				t.Fatalf("truth fixture rows=%d want=%d", len(values), len(rows))
			}
			var truths, falses, unknowns int
			for _, value := range values {
				if !sameRepeatEpoch(value.Folded, value.Original) {
					t.Fatalf("SQL truth value changed for row %d: fixture=%v", value.ID, rows[value.ID-1])
				}
				if value.Folded == nil {
					unknowns++
				} else if *value.Folded == 1 {
					truths++
				} else if *value.Folded == 0 {
					falses++
				} else {
					t.Fatalf("not a SQL truth value: %d", *value.Folded)
				}
			}
			if profile == "unlimited" {
				if truths != len(rows) {
					t.Fatal("unlimited scope filtered rows")
				}
			} else if truths == 0 || falses == 0 || unknowns == 0 {
				t.Fatalf("missing truth-value coverage: true=%d false=%d null=%d", truths, falses, unknowns)
			}
			t.Logf("rows=%d true=%d false=%d null=%d", len(values), truths, falses, unknowns)
		})
	}
}

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
			e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: limit}}, "peer:detail": {stage: "detail", config: StageConfig{Site: "peer", RepeatLimit: limit}}}}
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
		e := &Engine{stages: map[string]*stageInfo{"limited:detail": {stage: "detail", config: StageConfig{Site: "limited", RepeatLimit: limit}}, "peer:detail": {stage: "detail", config: StageConfig{Site: "peer", RepeatLimit: limit}}}}
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

func TestRepeatScheduleMySQLMixedCapGroups(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := &Engine{stages: map[string]*stageInfo{
		"a:detail":    {stage: "detail", config: StageConfig{Site: "a", RepeatLimit: 3}},
		"b:detail":    {stage: "detail", config: StageConfig{Site: "b", RepeatLimit: 3}},
		"c:detail":    {stage: "detail", config: StageConfig{Site: "c", RepeatLimit: 5}},
		"a:catalog":   {stage: "catalog", config: StageConfig{Site: "a", RepeatLimit: 2}},
		"d:catalog":   {stage: "catalog", config: StageConfig{Site: "d", RepeatLimit: 2}},
		"free:detail": {stage: "detail", config: StageConfig{Site: "free"}},
	}}
	if err := db.Connection(func(conn *gorm.DB) error {
		tx := conn.Session(&gorm.Session{NewDB: true})
		e.db = tx
		defer func() { e.db = nil }()
		if err := tx.Exec("SET timestamp = 1794614400").Error; err != nil {
			return err
		}
		defer func() {
			if err := tx.Exec("SET timestamp = 0").Error; err != nil {
				t.Error(err)
			}
		}()
		for _, tc := range []struct {
			name     string
			site     any
			stage    string
			round    any
			eligible bool
		}{
			{"a_below", "a", "detail", 2, true}, {"a_at", "a", "detail", 3, false},
			{"b_at", "b", "detail", 3, false}, {"b_null", "b", "detail", nil, false},
			{"c_different_limit", "c", "detail", 3, true}, {"c_at", "c", "detail", 5, false},
			{"a_other_stage", "a", "catalog", 2, false}, {"d_below", "d", "catalog", 1, true},
			{"d_at", "d", "catalog", 2, false}, {"d_unlimited_stage", "d", "detail", 100, true},
			{"free", "free", "detail", nil, true}, {"unknown", "unknown", "detail", nil, true},
			{"empty_stage", "a", "", nil, true}, {"unknown_stage", "a", "unknown", 100, true},
			{"null_site_below_all", nil, "detail", 2, true}, {"null_site_between_limits", nil, "detail", 3, false},
			{"null_site_null_round", nil, "detail", nil, false}, {"null_site_unknown_stage", nil, "unknown", nil, true},
		} {
			for _, status := range []any{2, 0, nil} {
				for _, at := range []any{nil, "2020-01-01 00:00:00", "2027-01-01 00:00:00"} {
					t.Run(fmt.Sprintf("%s/status_%v/at_%v", tc.name, status, at), func(t *testing.T) {
						insertRepeatScheduleRows(t, tx, repeatScheduleRow{status: status, at: at, round: tc.round})
						if err := tx.Exec("UPDATE crawler_tasks SET site = ?, stage = ?", tc.site, tc.stage).Error; err != nil {
							t.Fatal(err)
						}
						got, old := repeatScheduleEpoch(t, e, tx, false), repeatScheduleEpoch(t, e, tx, true)
						future := at == "2027-01-01 00:00:00"
						eligible := tc.eligible && (status == 2 || future)
						want := int64(1794614400)
						if at != nil {
							if err := tx.Raw("SELECT CAST(UNIX_TIMESTAMP(?) AS SIGNED)", at).Scan(&want).Error; err != nil {
								t.Fatal(err)
							}
						}
						if !sameRepeatEpoch(got, old) || (got != nil) != eligible || got != nil && *got != want {
							t.Fatal("grouping changed scheduling qualification/epoch")
						}
						for _, force := range []bool{false, true} {
							query := e.repeatQueueQuery()
							if force {
								query = e.repeatForceQueueQuery()
							}
							var count int64
							if err := query().Count(&count).Error; err != nil {
								t.Fatal(err)
							}
							wantCount := int64(0)
							if tc.eligible && status == 2 && (force || !future) {
								wantCount = 1
							}
							if count != wantCount {
								t.Fatalf("force=%v count=%d want=%d", force, count, wantCount)
							}
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
