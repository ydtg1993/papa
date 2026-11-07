package rulequality_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	q "github.com/ydtg1993/papa/v4/pkg/rulequality"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type adminRules struct{ agentadmin.Rules }
type blockingAdminModel struct {
	entered chan struct{}
	calls   atomic.Int32
}

type delayedAdminModel struct{ entered, release chan struct{} }

func (m delayedAdminModel) Complete(ctx context.Context, _ []model.Message, _ int) (model.Reply, error) {
	close(m.entered)
	<-m.release
	return model.Reply{}, ctx.Err()
}
func TestAgentCenterCloseTimeoutKeepsWorkerUntilItActuallyReturns(t *testing.T) {
	ctx := context.Background()
	m := delayedAdminModel{make(chan struct{}), make(chan struct{})}
	s, _, _, _, sel := centerHarness(t, m, nil)
	defer close(m.release)
	p, e := s.Preview(ctx, sel, "alice")
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Prepare(ctx, agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "delayed-close"}, "alice")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Start(ctx, agentadmin.StartInput{Selection: sel, Digest: p.Digest, ID: r.ID, Revision: r.Revision}, "alice"); e != nil {
		t.Fatal(e)
	}
	select {
	case <-m.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no worker")
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if e = s.Close(deadline); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	state, e := s.Request(ctx, r.ID)
	if e != nil || !state.Active || state.Status != q.Running {
		t.Fatalf("timeout invented completion %+v %v", state, e)
	}
}

func (m *blockingAdminModel) Complete(ctx context.Context, _ []model.Message, _ int) (model.Reply, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
	}
	<-ctx.Done()
	return model.Reply{}, ctx.Err()
}
func centerHarness(t *testing.T, provider model.Provider, quality func(*q.MemoryStore) agentadmin.Quality) (*agentadmin.Service, *agentadmin.Service, *q.MemoryStore, *agentstore.MemoryStore, agentadmin.Selection) {
	t.Helper()
	_, store, audit, replay, rules, _, input := bridgeHarness(t, nil)
	if provider == nil {
		provider = replay
	}
	dir := t.TempDir()
	bridgeSnapshot(t, dir, input.EvidenceIDs[0])
	cfg := agentadmin.Config{Audit: audit, Quality: store, Rules: adminRules{}, Profiles: []agentadmin.Profile{{ID: "approved", Revision: "1", Label: "Approved", ArchiveDir: dir, Goal: func(context.Context, q.Report) (crawlagent.Goal, error) { return input.Goal, nil }, Agent: crawlagent.Config{Model: provider, ModelName: "replay", Repository: rules, Blobs: rules.blobs, SamplePolicy: htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(b), "PRIVATE", "REDACTED")), nil
	}}, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://quality.example"}}}}, Budgets: crawlagent.Budgets{MaxModelCalls: 1}}}}}
	if quality != nil {
		cfg.Quality = quality(store)
	}
	s, err := agentadmin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := agentadmin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := s.Close(ctx); e != nil {
			t.Error(e)
		}
		if e := other.Close(ctx); e != nil {
			t.Error(e)
		}
	})
	return s, other, store, audit, agentadmin.Selection{ReportID: "report", ProfileID: "approved", EvidenceIDs: input.EvidenceIDs}
}
func TestAgentCenterConcurrentStartDetachedCancelAndClose(t *testing.T) {
	ctx := context.Background()
	m := &blockingAdminModel{entered: make(chan struct{})}
	s, other, store, _, sel := centerHarness(t, m, nil)
	preview, err := s.Preview(ctx, sel, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Plan.Budgets.MaxDuration != 180*time.Second || preview.Plan.WebBudgets.MaxPages == 0 {
		t.Fatal("effective defaults missing")
	}
	in := agentadmin.PrepareInput{Selection: sel, Digest: preview.Digest, OperationID: "oa-concurrent"}
	r, err := s.Prepare(ctx, in, "alice")
	if err != nil {
		t.Fatal(err)
	}
	again, err := other.Prepare(ctx, in, "alice")
	if err != nil || again.ID != r.ID || again.RunID != r.RunID {
		t.Fatalf("prepare %v", err)
	}
	start := agentadmin.StartInput{Selection: sel, Digest: preview.Digest, ID: r.ID, Revision: r.Revision}
	if _, e := s.Start(ctx, start, "bob"); e == nil {
		t.Fatal("operator reuse")
	}
	changed := start
	changed.Digest = "changed"
	if _, e := s.Start(ctx, changed, "alice"); !errors.Is(e, agentadmin.ErrReviewChanged) {
		t.Fatal(e)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, service := range []*agentadmin.Service{s, other} {
		wg.Add(1)
		go func(service *agentadmin.Service) {
			defer wg.Done()
			_, e := service.Start(requestCtx, start, "alice")
			if e != nil && !errors.Is(e, q.ErrConflict) {
				t.Errorf("start %v", e)
			}
		}(service)
	}
	wg.Wait()
	cancel()
	select {
	case <-m.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model not called")
	}
	if m.calls.Load() != 1 {
		t.Fatal("duplicate model")
	}
	persisted, e := store.Request(ctx, r.ID)
	if e != nil || persisted.Status != q.Running {
		t.Fatalf("HTTP cancellation leaked: %+v %v", persisted, e)
	}
	first, _ := s.Request(ctx, r.ID)
	owner := s
	if !first.Active {
		owner = other
	}
	active, _ := owner.Request(ctx, r.ID)
	if _, e := owner.Cancel(ctx, agentadmin.ControlInput{ID: r.ID, Revision: active.Revision, Confirmed: true}, "bob"); !errors.Is(e, agentadmin.ErrOwner) {
		t.Fatal(e)
	}
	if _, e := owner.Cancel(ctx, agentadmin.ControlInput{ID: r.ID, Revision: active.Revision, Confirmed: true}, "alice"); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(ctx); e != nil {
		t.Fatal(e)
	}
	if e := other.Close(ctx); e != nil {
		t.Fatal(e)
	}
	persisted, e = store.Request(ctx, r.ID)
	if e != nil || persisted.Status != q.Canceled || m.calls.Load() != 1 {
		t.Fatalf("cancel terminal %+v %v", persisted, e)
	}
}

type adminLostStart struct{ *q.MemoryStore }

func (s adminLostStart) StartRequest(ctx context.Context, id string, rev uint64, digest string) (q.Request, error) {
	_, e := s.MemoryStore.StartRequest(ctx, id, rev, digest)
	if e != nil {
		return q.Request{}, e
	}
	return q.Request{}, errors.New("lost acknowledgment")
}
func TestAgentCenterUnknownStartReadOnlyRestartAndExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	replay := model.NewReplay()
	s, _, store, audit, sel := centerHarness(t, replay, func(m *q.MemoryStore) agentadmin.Quality { return adminLostStart{m} })
	p, e := s.Preview(ctx, sel, "alice")
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Prepare(ctx, agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "unknown"}, "alice")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Start(ctx, agentadmin.StartInput{Selection: sel, Digest: p.Digest, ID: r.ID, Revision: r.Revision}, "alice"); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, _ := s.Request(ctx, r.ID)
		if !v.Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not end")
		}
		time.Sleep(time.Millisecond)
	}
	restarted, e := agentadmin.New(agentadmin.Config{Audit: audit, Quality: store})
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close(ctx)
	found, e := restarted.Operation(ctx, "unknown")
	if e != nil || found.Status != q.Running || found.RunID != r.RunID {
		t.Fatalf("restore %+v %v", found, e)
	}
	control := agentadmin.ControlInput{ID: r.ID, Revision: found.Revision, Confirmed: true}
	if _, e = restarted.Reconcile(ctx, control, "alice"); !errors.Is(e, agentadmin.ErrStopped) {
		t.Fatal(e)
	}
	recovery, e := agentadmin.New(agentadmin.Config{Audit: audit, Quality: store, WorkerStopped: func(context.Context, q.Request) bool { return true }})
	if e != nil {
		t.Fatal(e)
	}
	defer recovery.Close(ctx)
	done, e := recovery.Reconcile(ctx, control, "alice")
	if e != nil || done.Status != q.Interrupted || len(replay.Calls()) != 0 {
		t.Fatalf("recovery %+v %v", done, e)
	}
}
func TestAgentCenterPendingDismissAndSafeProjection(t *testing.T) {
	ctx := context.Background()
	s, _, _, audit, sel := centerHarness(t, nil, nil)
	p, e := s.Preview(ctx, sel, "alice")
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Prepare(ctx, agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "dismiss"}, "alice")
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Dismiss(ctx, agentadmin.ControlInput{ID: r.ID, Revision: r.Revision, Confirmed: true}, "alice")
	if e != nil || r.Status != q.Dismissed {
		t.Fatal(e)
	}
	run, e := audit.Start(ctx, agentstore.Run{ID: "safe-run", SnapshotJSON: `{"Goal":{"site":"site","context":"PRIVATE"},"Model":"model"}`})
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = audit.BeginStep(ctx, run.ID, run.Revision, agentstore.Step{Kind: "model", InputJSON: `{"body":"PRIVATE","tool":"read_evidence"}`}, `{}`)
	if e != nil {
		t.Fatal(e)
	}
	view, e := s.Run(ctx, run.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("raw audit leaked")
	}
}

func TestAgentCenterReconcileDoesNotHoldServiceLock(t *testing.T) {
	ctx := context.Background()
	s, _, store, audit, sel := centerHarness(t, nil, nil)
	p, err := s.Preview(ctx, sel, "alice")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Prepare(ctx, agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "guard-lock"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartRequest(ctx, r.ID, 1, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	recovery, err := agentadmin.New(agentadmin.Config{Audit: audit, Quality: store, WorkerStopped: func(context.Context, q.Request) bool {
		close(entered)
		<-release // deliberately uncooperative host: Close must still respect its timeout
		return true
	}})
	if err != nil {
		t.Fatal(err)
	}
	control := agentadmin.ControlInput{ID: r.ID, Revision: "2", Confirmed: true}
	reconciled := make(chan error, 1)
	go func() { _, e := recovery.Reconcile(ctx, control, "alice"); reconciled <- e }()
	<-entered
	read := make(chan error, 1)
	go func() { _, e := recovery.Request(ctx, r.ID); read <- e }()
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- recovery.Close(deadline) }()
	select {
	case e := <-read:
		if e != nil {
			t.Error(e)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("stop guard blocked unrelated request reads")
	}
	select {
	case e := <-closed:
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Errorf("Close returned %v before guard completed", e)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Close ignored deadline while stop guard held service lock")
	}
	close(release)
	<-reconciled
	if err := recovery.Close(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Request(ctx, r.ID)
	if err != nil || got.Status != running.Status || got.Revision != running.Revision {
		t.Fatalf("shutdown must cancel reconciliation before durable write: %+v %v", got, err)
	}
}

type deadlineSampleRules struct {
	*bridgeRules
	deadline chan bool
}

func (r *deadlineSampleRules) AddSample(ctx context.Context, _ rulestore.SampleInput, _ htmlfetch.SamplePolicy, _ rulestore.Actor) (rulestore.Sample, error) {
	_, bounded := ctx.Deadline()
	r.deadline <- bounded
	<-ctx.Done()
	return rulestore.Sample{}, ctx.Err()
}

func TestAgentCenterBudgetIncludesEvidenceImport(t *testing.T) {
	ctx := context.Background()
	_, store, audit, replay, rules, _, input := bridgeHarness(t, nil)
	wrapped := &deadlineSampleRules{rules, make(chan bool, 1)}
	dir := t.TempDir()
	bridgeSnapshot(t, dir, input.EvidenceIDs[0])
	s, err := agentadmin.New(agentadmin.Config{Audit: audit, Quality: store, Rules: adminRules{}, Profiles: []agentadmin.Profile{{
		ID: "bounded", Revision: "1", Label: "Bounded", ArchiveDir: dir,
		Goal: func(context.Context, q.Report) (crawlagent.Goal, error) { return input.Goal, nil },
		Agent: crawlagent.Config{Model: replay, ModelName: "replay", Repository: wrapped, Blobs: rules.blobs,
			SamplePolicy: htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) { return b, nil }},
			Web:          webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://quality.example"}}}},
			Budgets:      crawlagent.Budgets{MaxDuration: time.Second}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	sel := agentadmin.Selection{ReportID: "report", ProfileID: "bounded", EvidenceIDs: input.EvidenceIDs}
	p, err := s.Preview(ctx, sel, "alice")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Prepare(ctx, agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "bounded-import"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, agentadmin.StartInput{Selection: sel, Digest: p.Digest, ID: r.ID, Revision: r.Revision}, "alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case bounded := <-wrapped.deadline:
		if !bounded {
			t.Error("evidence import has no reviewed run deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("import did not start")
	}
	if t.Failed() {
		return
	}
	end := time.Now().Add(3 * time.Second)
	for {
		v, err := s.Request(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Active {
			if v.Status != q.Canceled || len(replay.Calls()) != 0 {
				t.Fatalf("unbounded import %+v", v)
			}
			break
		}
		if time.Now().After(end) {
			t.Fatal("evidence import outlived run budget")
		}
		time.Sleep(time.Millisecond)
	}
}
