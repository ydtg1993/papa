package executionpolicy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu             sync.Mutex
	rows           []History
	ops            map[string]Operation
	saves          int
	saveHook       func(context.Context, Update, string) (History, error)
	operationError error
}

func newMemoryStore() *memoryStore {
	d := InitialDocument()
	d.Breaker.Sites["fixture"] = d.Breaker.Defaults
	return &memoryStore{rows: []History{{Revision: 1, Document: d, Operator: "bootstrap", CreatedAt: time.Now()}}, ops: map[string]Operation{}}
}
func (s *memoryStore) Load(ctx context.Context) (History, error) {
	if e := ctx.Err(); e != nil {
		return History{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) == 0 {
		return History{}, ErrNotFound
	}
	return s.rows[len(s.rows)-1], nil
}
func (s *memoryStore) Save(ctx context.Context, u Update, operator string) (History, error) {
	s.mu.Lock()
	s.saves++
	hook := s.saveHook
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, u, operator)
	}
	return s.commit(ctx, u, operator)
}
func (s *memoryStore) commit(ctx context.Context, u Update, operator string) (History, error) {
	if e := ctx.Err(); e != nil {
		return History{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.rows[len(s.rows)-1]
	if current.Revision != u.ExpectedRevision {
		return History{}, ErrConflict
	}
	h := History{Revision: current.Revision + 1, Document: u.Document.Clone(), Operator: operator, Reason: u.Reason, OperationID: u.OperationID, CreatedAt: time.Now()}
	s.rows = append(s.rows, h)
	s.ops[u.OperationID] = Operation{ID: u.OperationID, Revision: h.Revision, Document: h.Document, Operator: operator, Reason: u.Reason, ExpectedRevision: u.ExpectedRevision, CreatedAt: h.CreatedAt}
	return h, nil
}
func (s *memoryStore) History(context.Context, int) ([]History, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, nil
}
func (s *memoryStore) Operation(ctx context.Context, id string) (Operation, error) {
	if e := ctx.Err(); e != nil {
		return Operation{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operationError != nil {
		return Operation{}, s.operationError
	}
	op, ok := s.ops[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

type memoryTarget struct {
	calls atomic.Int64
	hook  func(context.Context, uint64, Document) error
}

func (t *memoryTarget) Apply(ctx context.Context, revision uint64, d Document) error {
	t.calls.Add(1)
	if t.hook != nil {
		return t.hook(ctx, revision, d)
	}
	return nil
}
func (t *memoryTarget) Status() any { return t.calls.Load() }
func controllerFixture(t *testing.T) (*Controller, *memoryStore, *memoryTarget) {
	t.Helper()
	s := newMemoryStore()
	target := &memoryTarget{}
	c := New(s, target, []string{"fixture"})
	if e := c.Initialize(context.Background()); e != nil {
		t.Fatal(e)
	}
	return c, s, target
}
func candidate(c *Controller, id string) Update {
	v, _ := c.Read(context.Background())
	d := v.Document.Clone()
	d.Browser.PoolSize++
	return Update{ExpectedRevision: v.Revision, OperationID: id, Document: d, Reason: "fixture change"}
}
func codeIs(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }

func TestServiceCASIdempotencyAndMetadata(t *testing.T) {
	c, s, target := controllerFixture(t)
	u := candidate(c, "update-0001")
	v, e := c.Update(context.Background(), u, "alice")
	if e != nil || v.Revision != 2 || v.AppliedRevision != 2 || v.Operator != "alice" || v.UpdatedAt.IsZero() {
		t.Fatalf("view=%+v err=%v", v, e)
	}
	v, e = c.Update(context.Background(), u, "alice")
	if e != nil || v.Revision != 2 || target.calls.Load() != 2 || s.saves != 1 {
		t.Fatalf("retry repeated side effects: %+v %v calls=%d", v, e, target.calls.Load())
	}
	changed := u
	changed.Document = changed.Document.Clone()
	changed.Document.Browser.PoolSize++
	if _, e = c.Update(context.Background(), changed, "alice"); !errors.Is(e, ErrConflict) {
		t.Fatalf("operation collision: %v", e)
	}
	stale := u
	stale.OperationID = "update-0002"
	if _, e = c.Update(context.Background(), stale, "alice"); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale CAS: %v", e)
	}
	op, e := c.Operation(context.Background(), u.OperationID)
	if e != nil || op.Status != "applied" {
		t.Fatal(op, e)
	}
}
func TestServiceLostCommitConfirmationReconcilesWithoutSecondSave(t *testing.T) {
	c, s, target := controllerFixture(t)
	s.saveHook = func(ctx context.Context, u Update, actor string) (History, error) {
		_, e := s.commit(ctx, u, actor)
		if e != nil {
			return History{}, e
		}
		return History{}, errors.New("connection lost after commit")
	}
	u := candidate(c, "lost-commit-1")
	v, e := c.Update(context.Background(), u, "alice")
	if e != nil || v.Revision != 2 || v.AppliedRevision != 2 || s.saves != 1 || target.calls.Load() != 2 {
		t.Fatalf("not reconciled: %+v %v", v, e)
	}
}
func TestServiceUnknownCommitBlocksNewWritesThenReconciles(t *testing.T) {
	c, s, target := controllerFixture(t)
	s.saveHook = func(ctx context.Context, u Update, actor string) (History, error) {
		_, e := s.commit(ctx, u, actor)
		if e != nil {
			return History{}, e
		}
		s.mu.Lock()
		s.operationError = errors.New("database offline")
		s.mu.Unlock()
		return History{}, errors.New("commit confirmation lost")
	}
	u := candidate(c, "unknown-0001")
	v, e := c.Update(context.Background(), u, "alice")
	if !codeIs(e, "unavailable") || v.Status != "pending" {
		t.Fatalf("unknown result not exposed: %+v %v", v, e)
	}
	next := u
	next.OperationID = "unknown-0002"
	if _, e = c.Update(context.Background(), next, "alice"); !codeIs(e, "unavailable") || s.saves != 1 {
		t.Fatal("new write overtook unknown commit", e)
	}
	s.mu.Lock()
	s.operationError = nil
	s.mu.Unlock()
	v, e = c.Update(context.Background(), u, "alice")
	if e != nil || v.Status != "applied" || v.Revision != 2 || s.saves != 1 || target.calls.Load() != 2 {
		t.Fatalf("reconcile: %+v %v", v, e)
	}
}
func TestServiceFailedApplyBlocksWritesAndRetryUsesSavedVersion(t *testing.T) {
	c, s, target := controllerFixture(t)
	target.hook = func(context.Context, uint64, Document) error { return errors.New("component unavailable") }
	u := candidate(c, "apply-fail-1")
	v, e := c.Update(context.Background(), u, "alice")
	if !codeIs(e, "unavailable") || v.Status != "degraded" || v.Revision != 2 || v.AppliedRevision != 1 {
		t.Fatalf("degraded state: %+v %v", v, e)
	}
	if _, e = c.Update(context.Background(), candidate(c, "apply-fail-2"), "alice"); !codeIs(e, "unavailable") {
		t.Fatal("degraded accepted write")
	}
	target.hook = nil
	v, e = c.Update(context.Background(), u, "alice")
	if e != nil || v.AppliedRevision != 2 || s.saves != 1 {
		t.Fatalf("retry: %+v %v", v, e)
	}
}
func TestServiceShutdownDuringSaveKeepsSavedForRestart(t *testing.T) {
	c, s, target := controllerFixture(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	s.saveHook = func(ctx context.Context, u Update, actor string) (History, error) {
		close(entered)
		<-finish
		return s.commit(ctx, u, actor)
	}
	u := candidate(c, "shutdown-001")
	done := make(chan error, 1)
	go func() { _, e := c.Update(context.Background(), u, "alice"); done <- e }()
	<-entered
	c.BeginShutdown()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := c.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-flight Go write was not retained: %v", err)
	}
	cancelDrain()
	v, e := c.Read(context.Background())
	if e != nil || v.Status != "stopping" {
		t.Fatal(v, e)
	}
	close(finish)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, _ = c.Read(context.Background())
	if v.Revision != 2 || v.AppliedRevision != 1 || target.calls.Load() != 1 {
		t.Fatalf("shutdown applied new policy: %+v", v)
	}
	if _, e = c.Update(context.Background(), u, "alice"); !codeIs(e, "stopping") {
		t.Fatal(e)
	}
	restarted := New(s, target, []string{"fixture"})
	if e = restarted.Initialize(context.Background()); e != nil {
		t.Fatal(e)
	}
	v, _ = restarted.Read(context.Background())
	if v.AppliedRevision != 2 || v.Status != "applied" {
		t.Fatal(v)
	}
}
func TestServiceApplyingReadableAndClientCancellationAfterCommit(t *testing.T) {
	c, _, target := controllerFixture(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	target.hook = func(ctx context.Context, _ uint64, _ Document) error { close(entered); <-finish; return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := candidate(c, "cancel-00001")
	done := make(chan error, 1)
	go func() { _, e := c.Update(ctx, u, "alice"); done <- e }()
	<-entered
	cancel()
	v, _ := c.Read(context.Background())
	if v.Status != "applying" || v.Revision != 2 || v.AppliedRevision != 1 {
		t.Fatalf("applying not visible: %+v", v)
	}
	close(finish)
	if e := <-done; e != nil {
		t.Fatalf("request cancellation prevented committed publication: %v", e)
	}
}
func TestServiceDetachedDocumentsAndTargetInputs(t *testing.T) {
	c, s, target := controllerFixture(t)
	v, _ := c.Read(context.Background())
	delete(v.Document.Breaker.Sites, "fixture")
	v.Sites[0] = "wrong"
	v2, _ := c.Read(context.Background())
	if _, ok := v2.Document.Breaker.Sites["fixture"]; !ok || v2.Sites[0] != "fixture" {
		t.Fatal("view aliases controller")
	}
	hs, _ := c.History(context.Background(), 10)
	delete(hs[0].Document.Breaker.Sites, "fixture")
	h, _ := s.Load(context.Background())
	if _, ok := h.Document.Breaker.Sites["fixture"]; !ok {
		t.Fatal("history aliases store")
	}
	target.hook = func(_ context.Context, _ uint64, d Document) error { delete(d.Breaker.Sites, "fixture"); return nil }
	u := candidate(c, "detached-001")
	if _, e := c.Update(context.Background(), u, "alice"); e != nil {
		t.Fatal(e)
	}
	delete(u.Document.Breaker.Sites, "fixture")
	op, _ := c.Operation(context.Background(), u.OperationID)
	delete(op.Document.Breaker.Sites, "fixture")
	raw, _ := s.Operation(context.Background(), u.OperationID)
	v, _ = c.Read(context.Background())
	if len(raw.Document.Breaker.Sites) != 1 || len(v.Document.Breaker.Sites) != 1 {
		t.Fatal("mutable document escaped")
	}
}
func TestServiceConcurrentCASAndRollback(t *testing.T) {
	c, s, _ := controllerFixture(t)
	base := candidate(c, "concurrent-00")
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u := base
			u.OperationID = fmt.Sprintf("concurrent-%02d", i)
			_, e := c.Update(context.Background(), u, "alice")
			if e == nil {
				successes.Add(1)
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 || s.saves != 1 {
		t.Fatalf("CAS successes=%d saves=%d", successes.Load(), s.saves)
	}
	v, e := c.Rollback(context.Background(), Rollback{ExpectedRevision: 2, TargetRevision: 1, OperationID: "rollback-001", Reason: "restore"}, "alice")
	if e != nil || v.Revision != 3 || v.Document.Browser.PoolSize != InitialDocument().Browser.PoolSize {
		t.Fatal(v, e)
	}
}
func TestServiceInitializationMissingAndCancelled(t *testing.T) {
	s := newMemoryStore()
	s.rows = nil
	c := New(s, nil, nil)
	if e := c.Initialize(context.Background()); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	c, s, _ = controllerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Update(ctx, candidate(c, "cancelled-01"), "alice"); !errors.Is(e, context.Canceled) || s.saves != 0 {
		t.Fatal(e)
	}
}

func TestServiceUnknownUnconfirmedOperationIsNotBlindlyResubmitted(t *testing.T) {
	c, s, _ := controllerFixture(t)
	s.saveHook = func(context.Context, Update, string) (History, error) {
		return History{}, errors.New("transaction outcome unknown")
	}
	u := candidate(c, "unconfirmed-1")
	for i := 0; i < 2; i++ {
		v, e := c.Update(context.Background(), u, "alice")
		if !codeIs(e, "unavailable") || v.Status != "pending" {
			t.Fatal(v, e)
		}
	}
	if s.saves != 1 {
		t.Fatal("unconfirmed operation submitted twice")
	}
}
func TestServiceExternalCASConflictReturnsLatestPolicy(t *testing.T) {
	c, s, target := controllerFixture(t)
	u := candidate(c, "external-001")
	external := u
	external.OperationID = "external-002"
	external.Document = external.Document.Clone()
	external.Document.Browser.PoolSize = 9
	if _, e := s.commit(context.Background(), external, "other"); e != nil {
		t.Fatal(e)
	}
	v, e := c.Update(context.Background(), u, "alice")
	if !errors.Is(e, ErrConflict) || v.Revision != 2 || v.Document.Browser.PoolSize != 9 || v.AppliedRevision != 2 || target.calls.Load() != 2 {
		t.Fatalf("stale conflict state: %+v %v", v, e)
	}
}
func TestServiceCancelledCommitConfirmationStillPublishes(t *testing.T) {
	c, s, _ := controllerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.saveHook = func(ctx context.Context, u Update, actor string) (History, error) {
		_, e := s.commit(ctx, u, actor)
		if e != nil {
			return History{}, e
		}
		cancel()
		return History{}, context.Canceled
	}
	v, e := c.Update(ctx, candidate(c, "cancel-lost-1"), "alice")
	if e != nil || v.AppliedRevision != 2 {
		t.Fatal(v, e)
	}
}
func TestServiceValidateHasNoEffectsAndRejectsInvalidInput(t *testing.T) {
	c, s, target := controllerFixture(t)
	u := candidate(c, "validate-001")
	preview, e := c.Validate(context.Background(), u.Document)
	if e != nil || preview.CurrentRevision != 1 || s.saves != 0 || target.calls.Load() != 1 {
		t.Fatal(preview, e)
	}
	delete(preview.Document.Breaker.Sites, "fixture")
	if len(u.Document.Breaker.Sites) != 1 {
		t.Fatal("preview aliases caller")
	}
	u.Document.Browser.PoolSize = 0
	if _, e = c.Update(context.Background(), u, "alice"); !codeIs(e, "invalid") || s.saves != 0 {
		t.Fatal("invalid candidate saved", e)
	}
	if _, e = Decode([]byte(`{"schema_version":1,"extra":true}`)); e == nil {
		t.Fatal("unknown field accepted")
	}
	if _, e = Decode([]byte(`{"browser":{"max_idle_time":123}}`)); e == nil {
		t.Fatal("numeric duration accepted")
	}
}

func TestServiceOperationDoesNotClaimSkippedRevisionWasApplied(t *testing.T) {
	c, s, _ := controllerFixture(t)
	u := candidate(c, "supersede-01")
	if _, e := s.commit(context.Background(), u, "alice"); e != nil {
		t.Fatal(e)
	}
	u.ExpectedRevision = 2
	u.OperationID = "supersede-02"
	u.Document = u.Document.Clone()
	u.Document.Browser.PoolSize++
	if _, e := s.commit(context.Background(), u, "alice"); e != nil {
		t.Fatal(e)
	}
	restarted := New(s, &memoryTarget{}, []string{"fixture"})
	if e := restarted.Initialize(context.Background()); e != nil {
		t.Fatal(e)
	}
	op, e := restarted.Operation(context.Background(), "supersede-01")
	if e != nil || op.Status != "superseded" {
		t.Fatal(op, e)
	}
}

func TestServiceRejectsSameRevisionContentMutation(t *testing.T) {
	s := newMemoryStore()
	c := New(s, nil, []string{"fixture"})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.rows[0].Document = s.rows[0].Document.Clone()
	s.rows[0].Document.Browser.PoolSize++
	s.mu.Unlock()
	if err := c.Initialize(context.Background()); err == nil {
		t.Fatal("same revision content mutation accepted")
	}
	v, _ := c.Read(context.Background())
	if v.Document.Browser.PoolSize != 3 {
		t.Fatal("invalid mutation published")
	}
}
