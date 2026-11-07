package rulequality

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func exerciseFreeze(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	o, err := s.Record(ctx, storeObservation(prefix))
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Scope: o.Scope, From: o.CreatedAt.Add(-time.Second), Until: o.CreatedAt.Add(time.Hour), Limit: AdminSummaryLimit}
	w, err := s.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := FreezeInput{OperationID: prefix + "-freeze", Query: q, InputSHA256: preview.InputSHA256}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := FreezeReport(ctx, s, in, "reviewer")
			if e != nil || r.Operator != "reviewer" || r.OperationID != in.OperationID {
				t.Errorf("concurrent freeze: %+v %v", r, e)
			}
		}()
	}
	wg.Wait()
	frozen, err := ReportByOperation(ctx, s, in.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FreezeReport(ctx, s, in, "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner conflict %v", err)
	}
	second := storeObservation(prefix + "-second")
	second.Scope = o.Scope
	if _, err = s.Record(ctx, second); err != nil {
		t.Fatal(err)
	}
	again, err := FreezeReport(ctx, s, in, "reviewer")
	if err != nil || again.ID != frozen.ID || again.Cutoff != frozen.Cutoff {
		t.Fatalf("immutable retry %v", err)
	}
	in.OperationID += "-changed"
	if _, err := FreezeReport(ctx, s, in, "reviewer"); !errors.Is(err, ErrReportChanged) {
		t.Fatalf("stale review %v", err)
	}
	in.Query.Limit = 1
	if _, err := FreezeReport(ctx, s, in, "reviewer"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestFreezeReportConcurrentIdempotencyAndChangedWindow(t *testing.T) {
	exerciseFreeze(t, NewMemoryStore(), "freeze")
}
func TestFreezeReportMySQLReopen(t *testing.T) {
	s, db, p := qualityMySQL(t)
	exerciseFreeze(t, s, p)
	reopen, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	r, e := reopen.ReportOperation(context.Background(), p+"-freeze")
	if e != nil || r.Operator != "reviewer" {
		t.Fatal(e)
	}
	request := storeRequest(p+"-request", r.ID)
	if _, e = s.CreateRequest(context.Background(), request); e != nil {
		t.Fatal(e)
	}
	page, e := reopen.Requests(context.Background(), "", r.ID)
	if e != nil || len(page.Items) != 1 || page.Items[0].RunID != request.RunID {
		t.Fatalf("durable requests %+v %v", page, e)
	}
}

type freezeLateWindow struct {
	Store
	beforeQuery func()
}

func (s freezeLateWindow) Query(ctx context.Context, q Query) (Window, error) {
	s.beforeQuery()
	return s.Store.Query(ctx, q)
}

func exerciseFreezeConcurrentWinner(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	o, err := s.Record(ctx, storeObservation(prefix))
	if err != nil {
		t.Fatal(err)
	}
	query := Query{Scope: o.Scope, From: o.CreatedAt.Add(-time.Second), Until: o.CreatedAt.Add(time.Hour), Limit: AdminSummaryLimit}
	w, err := s.Query(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := FreezeInput{OperationID: prefix + "-winner", Query: query, InputSHA256: preview.InputSHA256}
	var winner Report
	delayed := freezeLateWindow{s, func() {
		winner, err = FreezeReport(ctx, s, in, "reviewer")
		if err != nil {
			t.Fatal(err)
		}
		late := storeObservation(prefix + "-late")
		late.Scope = o.Scope
		if _, err := s.Record(ctx, late); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := FreezeReport(ctx, delayed, in, "reviewer")
	if err != nil || got.ID != winner.ID || got.Cutoff != winner.Cutoff {
		t.Fatalf("concurrent committed report must win over changed live window: id=%s err=%v", got.ID, err)
	}
}

func TestFreezeReportConcurrentCommitBeforeChangedWindow(t *testing.T) {
	exerciseFreezeConcurrentWinner(t, NewMemoryStore(), "late-freeze")
}

func TestFreezeReportMySQLConcurrentCommitBeforeChangedWindow(t *testing.T) {
	s, _, prefix := qualityMySQL(t)
	exerciseFreezeConcurrentWinner(t, s, prefix)
}
