package agentstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func TestAuditAdminSQLBounds(t *testing.T) {
	db, e := gorm.Open(mysql.New(mysql.Config{DSN: "nobody@tcp(127.0.0.1:1)/unused", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if e != nil {
		t.Fatal(e)
	}
	stmt := runListQuery(db, RunFilter{From: time.Now(), Until: time.Now(), Before: "last", Status: Running}).Find(&[]models.AgentRun{}).Statement
	sql := stmt.SQL.String()
	for _, part := range []string{"created_at >= ? AND created_at < ?", "id < ?", "status = ?", "ORDER BY id DESC", "LIMIT ?"} {
		if !strings.Contains(sql, part) {
			t.Fatal(sql)
		}
	}
	stmt = stepPageQuery(db, "run", 100, 201).Find(&[]models.AgentStep{}).Statement
	if !strings.Contains(stmt.SQL.String(), "sequence > ? AND sequence <= ?") {
		t.Fatal(stmt.SQL.String())
	}
}

type adminStore interface {
	Store
	List(context.Context, RunFilter) (RunPage, error)
	Inspect(context.Context, string, uint64) (RunDetail, error)
}

func exerciseAuditAdmin(t *testing.T, s adminStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	start := time.Now().Add(-time.Minute)
	for i := 0; i < 52; i++ {
		if _, e := s.Start(ctx, Run{ID: fmt.Sprintf("%s-%03d", prefix, i), SnapshotJSON: `{"Goal":{"site":"` + prefix + `"}}`}); e != nil {
			t.Fatal(e)
		}
	}
	f := RunFilter{Site: prefix, From: start, Until: time.Now().Add(time.Minute)}
	p, e := s.List(ctx, f)
	if e != nil || p.Scanned != 50 || p.Next == "" {
		t.Fatalf("page %+v %v", p, e)
	} // other integration tests may share the time window; scan all candidates.
	total := len(p.Items)
	for p.Next != "" {
		f.Before = p.Next
		p, e = s.List(ctx, f)
		if e != nil {
			t.Fatal(e)
		}
		total += len(p.Items)
	}
	if total != 52 {
		t.Fatalf("site total %d", total)
	}
	id := prefix + "-steps"
	r, e := s.Start(ctx, Run{ID: id})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 102; i++ {
		var step Step
		r, step, e = s.BeginStep(ctx, id, r.Revision, Step{Kind: "model"}, `{}`)
		if e != nil {
			t.Fatal(e)
		}
		step.Status = StepSucceeded
		r, e = s.EndStep(ctx, id, r.Revision, step, `{}`)
		if e != nil {
			t.Fatal(e)
		}
	}
	d, e := s.Inspect(ctx, id, 0)
	if e != nil || len(d.Steps) != 100 || d.Next != 100 || d.Run.Revision != r.Revision {
		t.Fatalf("detail %+v %v", d, e)
	}
	d, e = s.Inspect(ctx, id, d.Next)
	if e != nil || len(d.Steps) != 2 || d.Next != 0 {
		t.Fatal(e)
	}
}
func TestMemoryAuditAdminPagination(t *testing.T) { exerciseAuditAdmin(t, NewMemoryStore(), "admin") }
func TestMySQLAuditAdminPaginationAndReopen(t *testing.T) {
	s, db, p := mysqlStore(t)
	exerciseAuditAdmin(t, s, p)
	reopen, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	d, e := reopen.Inspect(context.Background(), p+"-steps", 100)
	if e != nil || len(d.Steps) != 2 {
		t.Fatal(e)
	}
	// A missing trailing step must not be mistaken for a complete final page.
	if e = db.Where("run_id = ? AND sequence = ?", p+"-steps", 102).Delete(&models.AgentStep{}).Error; e != nil {
		t.Fatal(e)
	}
	if _, e = reopen.Inspect(context.Background(), p+"-steps", 100); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("missing audit tail accepted: %v", e)
	}
}
