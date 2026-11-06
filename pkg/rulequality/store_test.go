package rulequality

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func storeObservation(id string) Observation {
	return Observation{ID: id, Scope: Scope{Site: id, Stage: "catalog", RuleID: "quality", Version: "v1"}, RequestSHA256: strings.Repeat("a", 64), Operation: "complete", CompleteResponse: true, Evaluated: true, ExtractionOK: true, RawRows: 2, NewRows: 1, Fields: []FieldStat{{Name: "title", Kind: "string", Rows: 2, Present: 2, NonEmpty: 2}}, Page: 1}
}
func storeReport(id string, scope Scope) Report {
	return Report{ID: id, Query: Query{Scope: scope, From: time.Now().UTC().Add(-time.Hour), Until: time.Now().UTC().Add(time.Hour), Limit: 100}, InputSHA256: strings.Repeat("b", 64), Policy: Policy{MinPages: 20, MinRequests: 3}, Coverage: "recorded_observations_only", Folding: "latest_sequence_per_logical_key_else_observation_id; attempts_counted_separately", Verdict: Insufficient}
}
func storeRequest(id, report string) Request {
	return Request{ID: id, OperationID: id + "-op", ReportID: report, RunID: id + "-run", Operator: "human reviewer", Source: "quality regression test"}
}
func exerciseQualityStore(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	o := storeObservation(prefix)
	saved, err := s.Record(ctx, o)
	if err != nil || saved.Sequence == 0 || saved.CreatedAt.IsZero() {
		t.Fatalf("record: %+v %v", saved, err)
	}
	o.Fields[0].NonEmpty = 0
	q := Query{Scope: saved.Scope, From: saved.CreatedAt.Add(-time.Second), Until: saved.CreatedAt.Add(time.Second), Limit: 1}
	w, err := s.Query(ctx, q)
	if err != nil || len(w.Observations) != 1 || w.Observations[0].Fields[0].NonEmpty != 2 {
		t.Fatalf("detached write: %+v %v", w, err)
	}
	w.Observations[0].Fields[0].NonEmpty = 0
	again, err := s.Record(ctx, saved)
	if err != nil || !reflect.DeepEqual(saved, again) {
		t.Fatalf("stored retry: %+v %v", again, err)
	}
	raw := saved
	raw.Sequence = 0
	raw.CreatedAt = time.Time{}
	again, err = s.Record(ctx, raw)
	if err != nil || !reflect.DeepEqual(saved, again) {
		t.Fatalf("original retry: %+v %v", again, err)
	}
	raw.NewRows = 0
	if _, err := s.Record(ctx, raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("observation conflict: %v", err)
	}
	raw = storeObservation(prefix + "-forged")
	raw.Sequence = 7
	if _, err := s.Record(ctx, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged sequence: %v", err)
	}
	second := storeObservation(prefix + "-second")
	second.Scope = saved.Scope
	if _, err := s.Record(ctx, second); err != nil {
		t.Fatal(err)
	}
	w, err = s.Query(ctx, q)
	if err != nil || !w.Truncated || len(w.Observations) != 1 || w.Cutoff <= saved.Sequence {
		t.Fatalf("bounded cutoff: %+v %v", w, err)
	}
	q.Until = saved.CreatedAt
	w, err = s.Query(ctx, q)
	if err != nil || len(w.Observations) != 0 {
		t.Fatalf("half-open until: %+v %v", w, err)
	}
	q.From = saved.CreatedAt
	q.Until = saved.CreatedAt.Add(time.Hour)
	q.Scope.Version = "other"
	w, err = s.Query(ctx, q)
	if err != nil || len(w.Observations) != 0 {
		t.Fatalf("explicit version scope: %+v %v", w, err)
	}
	r := storeReport(prefix+"-report", saved.Scope)
	r.Cutoff = saved.Sequence
	r, err = s.SaveReport(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.SaveReport(ctx, r)
	if err != nil || !reflect.DeepEqual(r, retry) {
		t.Fatalf("immutable report retry: %+v %v", retry, err)
	}
	changed := r
	changed.Cutoff++
	if _, err := s.SaveReport(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable cutoff: %v", err)
	}
	changed = r
	changed.InputSHA256 = strings.Repeat("c", 64)
	if _, err := s.SaveReport(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable input: %v", err)
	}
	req := storeRequest(prefix+"-req", r.ID)
	made, err := s.CreateRequest(ctx, req)
	if err != nil || made.Revision != 1 || made.Status != Pending {
		t.Fatalf("create: %+v %v", made, err)
	}
	req.ID += "-new"
	req.RunID += "-new"
	dedup, err := s.CreateRequest(ctx, req)
	if err != nil || dedup.ID != made.ID || dedup.RunID != made.RunID {
		t.Fatalf("operation retains reserved run: %+v %v", dedup, err)
	}
	req.ReportID = "missing-changed"
	if _, err := s.CreateRequest(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation conflict: %v", err)
	}
	if _, err := s.FinishRequest(ctx, made.ID, made.Revision, AwaitingReview, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending cannot finish normally: %v", err)
	}
	running, err := s.StartRequest(ctx, made.ID, made.Revision, strings.Repeat("d", 64))
	if err != nil || running.Revision != 2 || running.Status != Running {
		t.Fatalf("start: %+v %v", running, err)
	}
	if _, err := s.StartRequest(ctx, made.ID, made.Revision, strings.Repeat("d", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("start replay: %v", err)
	}
	if _, err := s.FinishRequest(ctx, running.ID, running.Revision, Dismissed, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("running cannot dismiss: %v", err)
	}
	finished, err := s.FinishRequest(ctx, running.ID, running.Revision, AwaitingReview, `{"candidate":"candidate-1"}`)
	if err != nil || finished.Revision != 3 {
		t.Fatalf("finish: %+v %v", finished, err)
	}
	if _, err := s.FinishRequest(ctx, finished.ID, finished.Revision, Failed, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal cannot mutate: %v", err)
	}
	if _, err := s.PruneObservations(ctx, time.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, r.ID); err != nil {
		t.Fatalf("prune deleted report: %v", err)
	}
	if got, err := s.RequestByOperation(ctx, made.OperationID); err != nil || got.Status != AwaitingReview {
		t.Fatalf("prune deleted audit: %+v %v", got, err)
	}
	for _, status := range []string{Dismissed, NeedsEvidence, NeedsFixtures, Failed, Canceled, Interrupted} {
		request, err := s.CreateRequest(ctx, storeRequest(prefix+"-"+status, r.ID))
		if err != nil {
			t.Fatal(err)
		}
		if status != Dismissed {
			request, err = s.StartRequest(ctx, request.ID, request.Revision, strings.Repeat("e", 64))
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.FinishRequest(ctx, request.ID, request.Revision, status, `{}`); err != nil {
			t.Fatalf("terminal %s: %v", status, err)
		}
	}
}
func exerciseQualityCAS(t *testing.T, s Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	report, err := s.SaveReport(ctx, storeReport(prefix+"-report", storeObservation(prefix).Scope))
	if err != nil {
		t.Fatal(err)
	}
	const count = 12
	var wg sync.WaitGroup
	results := make(chan Request, count)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			proposal := storeRequest(fmt.Sprintf("%s-%d", prefix, i), report.ID)
			proposal.OperationID = prefix + "-op"
			r, err := s.CreateRequest(ctx, proposal)
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent create: %v", err)
	}
	var request Request
	for r := range results {
		if request.ID != "" && r.RunID != request.RunID {
			t.Error("duplicate run identity")
		}
		request = r
	}
	var winners atomic.Int64
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.StartRequest(ctx, request.ID, request.Revision, strings.Repeat("f", 64))
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("CAS: %v", err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("model reservations=%d", winners.Load())
	}
}
func TestQualityMemoryImmutableLifecycleAndCAS(t *testing.T) {
	s := NewMemoryStore()
	exerciseQualityStore(t, s, "memory")
	exerciseQualityCAS(t, s, "memorycas")
}
func TestQualityStoreValidationAndBoundaries(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	base := storeObservation("valid")
	for name, mutate := range map[string]func(*Observation){"request privacy": func(o *Observation) { o.RequestSHA256 = "https://private.test/?token=secret" }, "logical privacy": func(o *Observation) { o.LogicalKey = "cookie=secret" }, "raw error": func(o *Observation) { o.FailureKind = "remote login failed for account" }, "negative rows": func(o *Observation) { o.RawRows = -1 }, "invalid denominator": func(o *Observation) { o.Fields[0].Present = 3 }, "fields cap": func(o *Observation) { o.Fields = make([]FieldStat, MaxFields+1) }, "timestamp": func(o *Observation) { o.CreatedAt = time.Now() }} {
		t.Run(name, func(t *testing.T) {
			o := detached(base)
			mutate(&o)
			if _, err := s.Record(ctx, o); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted invalid record: %v", err)
			}
		})
	}
	q := storeReport("r", base.Scope).Query
	for _, limit := range []int{-1, MaxObservations + 1} {
		q.Limit = limit
		if _, err := s.Query(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid query limit %d: %v", limit, err)
		}
	}
	nan := math.NaN()
	if validatePolicy(Policy{MinPages: 1, MinRequests: 1, MissingRate: &nan}) == nil {
		t.Fatal("NaN threshold")
	}
	for _, limit := range []int{0, -1, MaxObservations + 1} {
		if _, err := s.PruneObservations(ctx, time.Now(), limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("prune bound: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Record(canceled, base); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestQualityMemoryPrunePreservesCutoffAndDetachedReports(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.now = func() time.Time { return now }
	o := storeObservation("prune")
	saved, err := s.Record(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	r := storeReport("prune-report", o.Scope)
	r.Policy.MinRows = new(int)
	r.Cutoff = saved.Sequence
	stored, err := s.SaveReport(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	*r.Policy.MinRows = 42
	*stored.Policy.MinRows = 19
	read, err := s.Report(ctx, r.ID)
	if err != nil || *read.Policy.MinRows != 0 {
		t.Fatalf("report aliases: %+v %v", read, err)
	}
	if n, err := s.PruneObservations(ctx, now, 1); err != nil || n != 0 {
		t.Fatalf("prune boundary %d %v", n, err)
	}
	if n, err := s.PruneObservations(ctx, now.Add(time.Microsecond), 1); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	o.ID = "after-prune"
	saved2, err := s.Record(ctx, o)
	if err != nil || saved2.Sequence <= saved.Sequence {
		t.Fatalf("sequence reused: %+v %v", saved2, err)
	}
}
