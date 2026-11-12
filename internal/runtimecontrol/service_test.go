package runtimecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"sync"
	"testing"
	"time"
)

type fixtureRuntime struct{ controller *governance.Controller }

func (r *fixtureRuntime) ResolveRuntime(v config.RuntimeConfig) (governance.Config, map[string]time.Duration, error) {
	c := governance.DefaultConfig()
	c.Defaults = governance.MergePolicy(c.Defaults, v.Defaults)
	c.Sites = map[string]governance.PolicyPatch{"fixture": {}}
	for k, p := range v.Sites {
		if k != "fixture" {
			return c, nil, errors.New("unknown site")
		}
		c.Sites[k] = p
	}
	return c, nil, c.Validate()
}
func (r *fixtureRuntime) Governance() *governance.Controller { return r.controller }
func (r *fixtureRuntime) InstallGovernance(c *governance.Controller, _ map[string]time.Duration) {
	r.controller = c
}
func (r *fixtureRuntime) GetProxy() *proxy.Manager        { return nil }
func (r *fixtureRuntime) Site(k string) (core.Site, bool) { return core.Site{Key: k}, k == "fixture" }

type fixtureStore struct {
	mu               sync.Mutex
	raw              json.RawMessage
	revision         uint64
	saveErr, loadErr error
	commitOnError    bool
	writes           int
	entered, release chan struct{}
}

func (f *fixtureStore) Load(context.Context) (json.RawMessage, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(json.RawMessage(nil), f.raw...), f.revision, f.loadErr
}
func (f *fixtureStore) Save(ctx context.Context, d Document, revision uint64, _ string) (uint64, error) {
	if f.entered != nil {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if revision != f.revision {
		return 0, ErrConflict
	}
	if f.saveErr != nil && !f.commitOnError {
		return 0, f.saveErr
	}
	f.raw, _ = json.Marshal(d)
	f.revision++
	return f.revision, f.saveErr
}
func (f *fixtureStore) History(context.Context) ([]models.RuntimePolicyHistory, error) {
	return nil, nil
}
func (f *fixtureStore) HistoryRevision(context.Context, uint64) (models.RuntimePolicyHistory, error) {
	return models.RuntimePolicyHistory{}, ErrNotFound
}
func fixture(t *testing.T) (*Service, *fixtureStore, *fixtureRuntime) {
	t.Helper()
	r := &fixtureRuntime{}
	f := &fixtureStore{}
	s := New(r, f, config.RuntimeConfig{})
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, f, r
}
func update(t *testing.T, revision uint64, raw string) Update {
	t.Helper()
	d, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return Update{Revision: revision, Document: d}
}
func TestLostAcknowledgementIsConfirmedWithoutReplayingWrite(t *testing.T) {
	s, f, r := fixture(t)
	f.saveErr = errors.New("connection lost after commit")
	f.commitOnError = true
	out := s.Save(context.Background(), update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`), "operator")
	if out.Kind != Success || s.Revision() != 1 || s.Uncertain() || f.writes != 1 || r.controller.Effective("fixture").Request.MaxInflight != 2 {
		t.Fatalf("unconfirmed or replayed: %+v writes=%d", out, f.writes)
	}
}
func TestUncertainWriteCannotRelaxPolicyUntilDurableReconciliation(t *testing.T) {
	s, f, r := fixture(t)
	ctx := context.Background()
	if out := s.Save(ctx, update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`), ""); out.Kind != Success {
		t.Fatal(out)
	}
	f.saveErr = errors.New("offline")
	f.loadErr = errors.New("offline")
	if out := s.Save(ctx, Update{Revision: 1, Document: Document{SchemaVersion: 1}}, ""); out.Kind != Unavailable {
		t.Fatal(out)
	}
	if out := s.Read(ctx); out.Kind != Unavailable || !s.Uncertain() || r.controller.Effective("fixture").Request.MaxInflight != 2 {
		t.Fatal(out)
	}
	before := f.writes
	if out := s.Save(ctx, Update{Revision: 1, Document: Document{SchemaVersion: 1}}, ""); out.Kind != Unavailable || f.writes != before {
		t.Fatal("uncertain write replayed")
	}
	f.loadErr = nil
	f.saveErr = nil
	if out := s.Read(ctx); out.Kind != Success || s.Uncertain() {
		t.Fatal(out)
	}
	if out := s.Save(ctx, Update{Revision: 1, Document: Document{SchemaVersion: 1}}, ""); out.Kind != Success || r.controller.Effective("fixture").Request.MaxInflight != 0 {
		t.Fatal(out)
	}
}
func TestCASWinnerReconcilesAndRejectsRollbackOrSameRevisionMutation(t *testing.T) {
	s, f, r := fixture(t)
	ctx := context.Background()
	f.raw = []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":3}}}}}`)
	f.revision = 4
	if out := s.Save(ctx, Update{Document: Document{SchemaVersion: 1}}, ""); out.Kind != Conflict || !s.Uncertain() {
		t.Fatal(out)
	}
	if out := s.Read(ctx); out.Kind != Success || s.Revision() != 4 || r.controller.Effective("fixture").Request.MaxInflight != 3 {
		t.Fatal(out)
	}
	f.saveErr = errors.New("offline")
	f.loadErr = errors.New("offline")
	_ = s.Save(ctx, Update{Revision: 4, Document: Document{SchemaVersion: 1}}, "")
	f.loadErr = nil
	f.revision = 3
	if out := s.Read(ctx); out.Kind != Unavailable || s.Revision() != 4 {
		t.Fatal("rollback accepted", out)
	}
	f.revision = 4
	f.raw = []byte(`{"schema_version":1,"overrides":{}}`)
	if out := s.Read(ctx); out.Kind != Unavailable || r.controller.Effective("fixture").Request.MaxInflight != 3 {
		t.Fatal("same revision mutation accepted", out)
	}
}
func TestPoolDeletionCannotCrossPolicyPersistenceAndPublication(t *testing.T) {
	s, f, _ := fixture(t)
	ctx := context.Background()
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	p, err := account.NewPools(ctx, m, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Hour, References: func(context.Context, account.PoolKey) (int64, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := account.PoolKey{Site: "fixture", Name: "primary"}
	saved, err := p.Upsert(ctx, account.PoolConfig{Key: key, Disabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AttachPools(p); err != nil {
		t.Fatal(err)
	}
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	policy := update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"pool":"primary"}}}}`)
	saveDone := make(chan Result, 1)
	go func() { saveDone <- s.Save(ctx, policy, "") }()
	<-f.entered
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- s.DeletePool(ctx, key, saved.Revision) }()
	select {
	case err := <-deleteDone:
		t.Fatalf("delete crossed in-flight publication: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(f.release)
	if out := <-saveDone; out.Kind != Success {
		t.Fatal(out)
	}
	if err := <-deleteDone; !errors.Is(err, account.ErrPoolReferenced) {
		t.Fatal("published reference lost", err)
	}
	if _, err = p.Get(key); err != nil {
		t.Fatal("referenced pool deleted", err)
	}
}
func TestReadDocumentIsDetachedFromOwnedPolicy(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	u := update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"weight":2}}}}`)
	if out := s.Save(ctx, u, ""); out.Kind != Success {
		t.Fatal(out)
	}
	*u.Document.Overrides.Sites["fixture"].Weight = 7
	doc := s.Read(ctx).Value["document"].(Document)
	*doc.Overrides.Sites["fixture"].Weight = 9
	if got := *s.Read(ctx).Value["document"].(Document).Overrides.Sites["fixture"].Weight; got != 2 {
		t.Fatal("snapshot exposed mutable service state", got)
	}
}

func TestPolicySnapshotStaysWithItsRevision(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	first := s.Save(ctx, update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`), "")
	if first.Kind != Success {
		t.Fatal(first)
	}
	second := s.Save(ctx, update(t, 1, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":7}}}}}`), "")
	if second.Kind != Success {
		t.Fatal(second)
	}
	snapshot, ok := first.Value["snapshot"].(governance.Snapshot)
	if !ok || len(snapshot.Sites) != 1 || snapshot.Sites[0].Policy.Request.MaxInflight != 2 || first.Value["revision"] != uint64(1) {
		t.Fatalf("snapshot must belong to the returned policy revision: %+v", first.Value)
	}
}

func TestPoolDeletionReconcilesUncertainPolicyBeforeCheckingReferences(t *testing.T) {
	s, f, _ := fixture(t)
	ctx := context.Background()
	p, err := account.NewPools(ctx, account.NewManager(htmlfetch.NewClient(htmlfetch.Config{})), account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Hour, References: func(context.Context, account.PoolKey) (int64, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := account.PoolKey{Site: "fixture", Name: "primary"}
	saved, err := p.Upsert(ctx, account.PoolConfig{Key: key, Disabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachPools(p); err != nil {
		t.Fatal(err)
	}
	f.commitOnError = true
	f.saveErr = errors.New("lost commit acknowledgement")
	f.loadErr = errors.New("database offline")
	out := s.Save(ctx, update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"pool":"primary"}}}}`), "")
	if out.Kind != Unavailable || !s.Uncertain() {
		t.Fatal(out)
	}
	if err := s.DeletePool(ctx, key, saved.Revision); err == nil {
		t.Fatal("deleted pool while durable references were unknown")
	}
	if _, err := p.Get(key); err != nil {
		t.Fatal(err)
	}
	f.loadErr = nil
	if err := s.DeletePool(ctx, key, saved.Revision); !errors.Is(err, account.ErrPoolReferenced) {
		t.Fatalf("durable reference was not reconciled: %v", err)
	}
	if s.Uncertain() || s.Revision() != 1 {
		t.Fatal("reconciliation did not publish confirmed revision")
	}
	if _, err := p.Get(key); err != nil {
		t.Fatal("referenced pool deleted", err)
	}
}

func TestRemoteAcknowledgementRejectsUncertainRevision(t *testing.T) {
	s, f, r := fixture(t)
	ctx := context.Background()
	r.controller.Observe("fixture", governance.Feedback{StatusCode: 429, RetryAfter: "86400"})
	before := r.controller.Snapshot().Sites[0]
	if !before.RemotePaused {
		t.Fatal("fixture did not pause for remote wait")
	}
	f.commitOnError = true
	f.saveErr = errors.New("lost commit acknowledgement")
	f.loadErr = errors.New("database offline")
	if out := s.Save(ctx, update(t, 0, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`), ""); out.Kind != Unavailable {
		t.Fatal(out)
	}
	if out := s.Acknowledge("fixture", 0); out.Kind != Unavailable {
		t.Fatal("accepted unconfirmed revision", out)
	}
	if !r.controller.Snapshot().Sites[0].RemotePaused {
		t.Fatal("uncertain acknowledgement released pause")
	}
	f.loadErr = nil
	if out := s.Read(ctx); out.Kind != Success {
		t.Fatal(out)
	}
	if out := s.Acknowledge("fixture", 0); out.Kind != Conflict {
		t.Fatal("accepted stale revision after reconciliation", out)
	}
	if out := s.Acknowledge("fixture", 1); out.Kind != Success {
		t.Fatal(out)
	}
	after := r.controller.Snapshot().Sites[0]
	if after.RemotePaused || !after.CooldownUntil.Equal(before.CooldownUntil) {
		t.Fatal("confirmed acknowledgement must preserve remote deadline", after)
	}
}
