package rulequality

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestQualityAdminSQLKeysetsAreBounded(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(localhost)/dry", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	f := AdminFilter{Before: 100, BeforeReport: "report", From: time.Now(), Until: time.Now().Add(time.Hour)}
	var observations []models.RuleObservation
	q := adminObservationQuery(db, f, 200).Find(&observations)
	for _, part := range []string{"FROM `crawler_rule_observations`", "sequence <= ?", "created_at >= ? AND created_at < ?", "sequence < ?", "ORDER BY sequence DESC LIMIT ?"} {
		if !strings.Contains(q.Statement.SQL.String(), part) {
			t.Fatalf("observation query: %s", q.Statement.SQL.String())
		}
	}
	if !reflect.DeepEqual(q.Statement.Vars, []any{uint64(200), f.From.UTC(), f.Until.UTC(), uint64(100), 51}) {
		t.Fatalf("observation bounds: %#v", q.Statement.Vars)
	}
	var reports []models.RuleQualityReport
	q = adminReportQuery(db, f).Find(&reports)
	if !strings.Contains(q.Statement.SQL.String(), "id < ? ORDER BY id DESC LIMIT ?") || len(q.Statement.Vars) != 4 || q.Statement.Vars[3] != 51 {
		t.Fatalf("report bounds: %s %#v", q.Statement.SQL.String(), q.Statement.Vars)
	}
}

func TestQualityAdminFilterBounds(t *testing.T) {
	f := AdminFilter{From: time.Now(), Until: time.Now().Add(time.Hour)}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*AdminFilter){func(f *AdminFilter) { f.Site = "bad\tkey" }, func(f *AdminFilter) { f.Until = f.From }, func(f *AdminFilter) { f.Until = f.From.Add(8 * 24 * time.Hour) }, func(f *AdminFilter) { f.Before = 1 }, func(f *AdminFilter) { f.Before = 3; f.Cutoff = 2 }, func(f *AdminFilter) { f.BeforeReport = "bad/id" }} {
		bad := f
		mutate(&bad)
		if !errors.Is(bad.Validate(), ErrInvalid) {
			t.Fatalf("accepted invalid filter: %+v", bad)
		}
	}
}
func TestQualityAdminMySQLCutoffFilteringRecoveryAndEvidence(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	start, err := qualityNow(db)
	if err != nil {
		t.Fatal(err)
	}
	var saved []Observation
	for i := 0; i < 55; i++ {
		o := storeObservation(fmt.Sprintf("%s-%02d", prefix, i))
		o.Scope.Site = prefix
		o.Scope.ReleaseID = "release"
		o.Scope.ReleaseGeneration = 9007199254740993
		got, err := s.Record(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, got)
	}
	f := AdminFilter{Site: prefix, From: start, Until: start.Add(time.Hour)}
	first, err := s.Observations(ctx, f)
	if err != nil || len(first.Items) != 50 || first.Scanned != 50 || first.Next != saved[5].Sequence || first.Cutoff != saved[54].Sequence {
		t.Fatalf("first: %+v %v", first, err)
	}
	newer := storeObservation(prefix + "-new")
	newer.Scope = saved[0].Scope
	added, err := s.Record(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	// New store instance restores the same cursor without process-local state.
	reopened, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	f.Before, f.Cutoff = first.Next, first.Cutoff
	second, err := reopened.Observations(ctx, f)
	if err != nil || len(second.Items) != 5 || second.Next != 0 || second.Cutoff != first.Cutoff {
		t.Fatalf("second: %+v %v", second, err)
	}
	for _, o := range second.Items {
		if o.Sequence == added.Sequence {
			t.Fatal("new insertion entered frozen page")
		}
	}
	f.Before = 0
	f.Site = "missing"
	empty, err := s.Observations(ctx, f)
	if err != nil || len(empty.Items) != 0 || empty.Scanned != 50 || empty.Next == 0 {
		t.Fatalf("filtered scan must retain next: %+v %v", empty, err)
	}
	q := Query{Scope: saved[0].Scope, From: start, Until: f.Until, Limit: AdminSummaryLimit}
	report, err := BuildReport(ctx, s, q, DefaultPolicy(), "")
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reopened.Observation(ctx, saved[0].ID, report.ID)
	if err != nil || !reflect.DeepEqual(saved[0], observation) {
		t.Fatalf("report association: %+v %v", observation, err)
	}
	unrelated := storeReport(prefix+"-unrelated", saved[0].Scope)
	unrelated, err = s.SaveReport(ctx, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Observation(ctx, saved[0].ID, unrelated.ID); !errors.Is(err, ErrEvidence) {
		t.Fatalf("unrelated report: %v", err)
	}
	for i, mutate := range []func(*Report){
		func(r *Report) { r.Query.Scope.ReleaseGeneration++ },
		func(r *Report) { r.Cutoff = saved[0].Sequence - 1 },
		func(r *Report) { r.Query.From = saved[0].CreatedAt.Add(time.Microsecond) },
		func(r *Report) { r.Evidence[0].EvidenceID = "wrong-evidence" },
	} {
		bad := detached(report)
		bad.ID = fmt.Sprintf("%s-mismatch-%d", prefix, i)
		bad.CreatedAt = time.Time{}
		mutate(&bad)
		bad, err = s.SaveReport(ctx, bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Observation(ctx, saved[0].ID, bad.ID); !errors.Is(err, ErrEvidence) {
			t.Fatalf("mismatch %d: %v", i, err)
		}
	}
	summary, err := s.Summary(ctx, q)
	if err != nil || summary.ID != "" || summary.Metrics.Attempts != 56 {
		t.Fatalf("ephemeral summary: %+v %v", summary, err)
	}
	q.Scope.ReleaseGeneration++
	summary, err = s.Summary(ctx, q)
	if err != nil || summary.Metrics.Attempts != 0 || summary.Verdict != Insufficient {
		t.Fatalf("release scope mixed: %+v %v", summary, err)
	}
	if err := db.Where("id = ?", saved[0].ID).Delete(&models.RuleObservation{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = s.Observation(ctx, saved[0].ID, report.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retained reference: %v", err)
	}
	if _, err = reopened.Report(ctx, report.ID); err != nil {
		t.Fatalf("report lost after observation pruning: %v", err)
	}
}
func TestQualityAdminMySQLReportsAndCorruption(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	start, err := qualityNow(db)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.Record(ctx, storeObservation(prefix))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 53; i++ {
		r := storeReport(fmt.Sprintf("%s-%02d", prefix, i), o.Scope)
		if _, err := s.SaveReport(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f := AdminFilter{Site: prefix, From: start, Until: start.Add(time.Hour)}
	p, err := s.Reports(ctx, f)
	if err != nil || len(p.Items) != 50 || p.Next != prefix+"-03" {
		t.Fatalf("report page: %+v %v", p, err)
	}
	f.BeforeReport = p.Next
	p, err = s.Reports(ctx, f)
	if err != nil || len(p.Items) != 3 || p.Next != "" {
		t.Fatalf("report continuation: %+v %v", p, err)
	}
	var row models.RuleObservation
	if err := db.Where("id = ?", o.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Model(&models.RuleObservation{}).Where("id = ?", o.ID).Update("payload_json", row.PayloadJSON)
	})
	db.Model(&models.RuleObservation{}).Where("id = ?", o.ID).Update("payload_json", "{}")
	if _, err = s.Observation(ctx, o.ID, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("detail corruption: %v", err)
	}
	if _, err = s.Observations(ctx, f); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("scan corruption: %v", err)
	}
	var reportRow models.RuleQualityReport
	db.Where("id = ?", prefix+"-52").Take(&reportRow)
	t.Cleanup(func() {
		db.Model(&models.RuleQualityReport{}).Where("id = ?", reportRow.ID).Update("payload_json", reportRow.PayloadJSON)
	})
	db.Model(&models.RuleQualityReport{}).Where("id = ?", reportRow.ID).Update("payload_json", "{}")
	f.BeforeReport = ""
	if _, err = s.Reports(ctx, f); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("report corruption: %v", err)
	}
}
