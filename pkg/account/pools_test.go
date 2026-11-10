package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func poolManager(t *testing.T, handler http.HandlerFunc, ids ...string) (*Manager, *httptest.Server, *atomic.Int32) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Header.Get("Authorization")) }
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	m := NewManager(client)
	checks := &atomic.Int32{}
	for _, id := range ids {
		id := id
		if err := m.Register(Key{Site: "s", ID: id}, Config{Origins: []string{srv.URL}, MaxConcurrent: 1, CheckInterval: time.Hour, Login: func(_ context.Context, c *Credentials) error { c.Headers["Authorization"] = "secret-" + id; return nil }, Check: func(context.Context, *Credentials) (bool, error) { checks.Add(1); return true, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	return m, srv, checks
}
func testPools(t *testing.T, m *Manager, store PoolStore) *Pools {
	t.Helper()
	p, err := NewPools(context.Background(), m, store, PoolOptions{ScanInterval: 5 * time.Millisecond, VerifyTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
func createPool(t *testing.T, p *Pools, name string, members ...string) PoolKey {
	t.Helper()
	key := PoolKey{Site: "s", Name: name}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: key, Members: members, Cooldown: time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	return key
}
func acquirePool(t *testing.T, p *Pools, key PoolKey) *Attempt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, err := p.Acquire(ctx, key, "")
	if err != nil {
		snapshot, _ := p.Get(key)
		t.Fatalf("acquire: %v snapshot=%+v", err, snapshot)
	}
	return a
}
func eventuallyPool(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pool condition did not settle")
}

func TestPoolStickySharedFailureNoReplay(t *testing.T) {
	var expired atomic.Bool
	var calls atomic.Int32
	m, srv, checks := poolManager(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if expired.Load() && r.Header.Get("Authorization") == "secret-a" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, r.Header.Get("Authorization"))
	}, "a", "b")
	// This test covers a provider's strong expiry contract; default 401 is
	// independently covered by the suspicious-state tests.
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.Classify = func(r *htmlfetch.Response, err error) Outcome {
		if r != nil && r.StatusCode == 401 {
			return Expired
		}
		return defaultClassify(r, err)
	}
	p := testPools(t, m, NewMemoryPoolStore())
	k1 := createPool(t, p, "one", "a", "b")
	k2 := createPool(t, p, "two", "a", "b")
	a1 := acquirePool(t, p, k1)
	a2 := acquirePool(t, p, k2)
	if a1.Account.ID != "a" || a2.Account.ID != "a" {
		t.Fatal("first member not selected")
	}
	for i := 0; i < 5; i++ {
		a := acquirePool(t, p, k1)
		if a.Account.ID != "a" || a.SelectionVersion != a1.SelectionVersion {
			t.Fatal("healthy pool rotated")
		}
	}
	expired.Store(true)
	before := calls.Load()
	_, err := m.Do(WithAttempt(context.Background(), a1), a1.Account, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, ErrExpired) || calls.Load() != before+1 {
		t.Fatalf("expired exchange replayed: err=%v calls=%d", err, calls.Load()-before)
	}
	b1 := acquirePool(t, p, k1)
	b2 := acquirePool(t, p, k2)
	if b1.Account.ID != "b" || b2.Account.ID != "b" {
		t.Fatal("shared invalid account selected again")
	}
	if p.ReportFailure(a1, Expired, 0) || p.ReportFailure(a2, Expired, 0) {
		t.Fatal("duplicate generation failure applied")
	}
	_, err = m.DoPool(WithAttempt(context.Background(), a2), k2, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, ErrAttemptChanged) {
		t.Fatalf("old task continued under new identity: %v", err)
	}
	beforeChecks := checks.Load()
	for i := 0; i < 5; i++ {
		p.Snapshot()
	}
	if checks.Load() != beforeChecks {
		t.Fatal("snapshot performed verification")
	}
	data, _ := json.Marshal(p.Snapshot())
	if strings.Contains(string(data), "secret-") {
		t.Fatal("credential leaked in snapshot")
	}
}

func TestPoolForbiddenDoesNotRotate(t *testing.T) {
	m, srv, _ := poolManager(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }, "a", "b")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	_, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	next := acquirePool(t, p, key)
	if next.Account != a.Account || next.SelectionVersion != a.SelectionVersion {
		t.Fatal("403 rotated account")
	}
}

func TestPoolConcurrentFailureAndReverifyRejectsLateIdentity(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.ReportFailure(a, Expired, 42) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("applied %d failures", won.Load())
	}
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("did not fail over")
	}
	snapshot, _ := p.Get(key)
	var revision uint64
	for _, v := range snapshot.Accounts {
		if v.Key.ID == "a" {
			revision = v.Management.Version
		}
	}
	if err := p.Verify(context.Background(), a.Account, revision); err != nil {
		t.Fatal(err)
	}
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Accounts[0].State == "available" })
	if p.ReportFailure(a, Expired, 42) {
		t.Fatal("late failure invalidated verified credentials")
	}
	if got := acquirePool(t, p, key); got.Account.ID != "b" {
		t.Fatal("reverify switched back to recovered account")
	}
	snapshot, _ = p.Get(key)
	if len(snapshot.History) != 2 || snapshot.History[1].TaskID != 42 {
		t.Fatalf("duplicate failover history: %+v", snapshot.History)
	}
}

func TestPoolSaturatedAccountWaitCancellationDoesNotRotate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m, srv, _ := poolManager(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		fmt.Fprint(w, "ok")
	}, "a", "b")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	done := make(chan error, 1)
	go func() {
		_, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL})
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(WithAttempt(context.Background(), a), 20*time.Millisecond)
	defer cancel()
	_, err := m.DoPool(ctx, key, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if next := acquirePool(t, p, key); next.Account.ID != "a" {
		t.Fatal("capacity triggered failover")
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	e, _ := m.get(a.Account)
	if len(e.slots) != 0 {
		t.Fatal("account permit leaked")
	}
}

type failingManagementStore struct {
	*MemoryPoolStore
	fail atomic.Bool
}

func (s *failingManagementStore) SaveManagementCAS(ctx context.Context, v ManagementState, expected uint64) error {
	if s.fail.Load() {
		return errors.New("secret backend unavailable")
	}
	return s.MemoryPoolStore.SaveManagementCAS(ctx, v, expected)
}
func TestPoolPersistenceFailureQuarantinesAndRetries(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	store := &failingManagementStore{MemoryPoolStore: NewMemoryPoolStore()}
	p := testPools(t, m, store)
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	beforeFailure, _ := p.Get(key)
	if err := p.Verify(context.Background(), Key{Site: "s", ID: "b"}, beforeFailure.Accounts[1].Management.Version); err != nil {
		t.Fatal(err)
	}
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Accounts[1].State == "available" })
	store.fail.Store(true)
	if !p.ReportFailure(a, Expired, 0) {
		t.Fatal("failure not accepted")
	}
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("failed account selected")
	}
	s, _ := p.Get(key)
	if s.PersistenceError == "" || s.Accounts[0].Persisted {
		t.Fatal("persistence failure hidden")
	}
	store.fail.Store(false)
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Accounts[0].Persisted })
	saved, err := store.LoadManagement(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range saved {
		if v.Key.ID == "a" {
			found = v.Quarantined && v.PendingInvalidation
		}
	}
	if !found {
		t.Fatal("pending invalidation did not persist")
	}
}

func TestPoolRestartPreservesInvalidAndRechecksCurrent(t *testing.T) {
	store := NewMemoryPoolStore()
	m, _, _ := poolManager(t, nil, "a", "b")
	p, err := NewPools(context.Background(), m, store, PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	p.ReportFailure(a, Expired, 0)
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("failed over incorrectly")
	}
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Accounts[0].Persisted })
	p.Close()
	m2, _, checks := poolManager(t, nil, "a", "b")
	p2 := testPools(t, m2, store)
	got := acquirePool(t, p2, key)
	if got.Account.ID != "b" || checks.Load() != 1 {
		t.Fatalf("restart reset invalid account or skipped check: %+v checks=%d", got, checks.Load())
	}
	if got.SelectionVersion != b.SelectionVersion {
		t.Fatal("restart rotated healthy selected account")
	}
}

func TestPoolCASAndDisabledWait(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "p", "a")
	_ = acquirePool(t, p, key)
	s, _ := p.Get(key)
	cfg := s.PoolConfig
	cfg.Disabled = true
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Upsert(context.Background(), cfg, s.Revision)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrPoolConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("CAS winners=%d", successes.Load())
	}
	pausedCtx, pausedCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer pausedCancel()
	if _, err := p.Acquire(pausedCtx, key, HTTP); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	empty := createPool(t, p, "empty")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(ctx, empty, HTTP); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPoolBrowserOnlyReadiness(t *testing.T) {
	m := NewManager(nil)
	key := Key{Site: "s", ID: "browser"}
	if err := m.Register(key, Config{Origins: []string{"https://example.test"}, Browser: &BrowserConfig{Pool: &browser.Pool{}, URL: "https://example.test", Login: func(context.Context, *rod.Page) error { return nil }, Check: func(context.Context, *rod.Page) (bool, error) { return true, nil }}}); err != nil {
		t.Fatal(err)
	}
	p := testPools(t, m, NewMemoryPoolStore())
	e, _ := m.get(key)
	e.mu.Lock()
	e.pageState = &browser.PageState{Origin: "https://example.test"}
	e.browserCheck = time.Now()
	e.mu.Unlock()
	pk := PoolKey{Site: "s", Name: "p"}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: pk, Transport: Browser, Members: []string{key.ID}}, 0); err != nil {
		t.Fatal(err)
	}
	a := acquirePool(t, p, pk)
	if a.Account != key || a.Transport != Browser {
		t.Fatal("browser-only account excluded")
	}
	s, _ := p.Get(pk)
	if s.Accounts[0].HTTPReady || !s.Accounts[0].BrowserReady {
		t.Fatal("transport readiness conflated")
	}
}

func TestPoolBrowserExpiredDoesNotReloginOrReplayCallback(t *testing.T) {
	s := newAccountBrowserSite(t)
	bp := accountBrowserPool(t, s.server.URL)
	m := NewManager(s.client)
	var expired atomic.Bool
	for _, name := range []string{"a", "b"} {
		cfg := s.config(bp, name)
		check := cfg.Browser.Check
		if name == "a" {
			cfg.Browser.Check = func(ctx context.Context, page *rod.Page) (bool, error) {
				if expired.Load() {
					return false, nil
				}
				return check(ctx, page)
			}
		}
		if err := m.Register(Key{Site: "s", ID: name}, cfg); err != nil {
			t.Fatal(err)
		}
	}
	p := testPools(t, m, NewMemoryPoolStore())
	key := PoolKey{Site: "s", Name: "browser"}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: key, Transport: Browser, Members: []string{"a", "b"}, Cooldown: time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	a := acquirePool(t, p, key)
	before := s.logins.Load()
	if a.Account.ID != "a" {
		snapshot, _ := p.Get(key)
		t.Fatalf("first browser account was not selected: %+v", snapshot)
	}
	expired.Store(true)
	var callbacks atomic.Int32
	err := m.WithPoolBrowser(WithAttempt(context.Background(), a), key, func(*rod.Page) error { callbacks.Add(1); return nil })
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want expired, got %v", err)
	}
	if callbacks.Load() != 0 {
		t.Fatal("expired browser ran callback")
	}
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" || s.logins.Load() != before+1 {
		t.Fatalf("expired A was relogged instead of selecting B: account=%s logins=%d", b.Account.ID, s.logins.Load()-before)
	}
	ctx := WithAttempt(context.Background(), b)
	if err = m.NavigatePoolPage(ctx, key, s.server.URL+"/content", browser.PageOptions{WaitJS: `localStorage.getItem('account') === 'b'`}, func(page *rod.Page) error {
		value, err := page.Eval(`() => localStorage.getItem('account')`)
		if err == nil && value.Value.Str() != "b" {
			return errors.New("account browser identity lost")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := m.CapturePoolRequest(ctx, key, s.server.URL+"/content", browser.CaptureOptions{Match: func(r browser.CapturedRequest) bool { return strings.HasSuffix(r.URL, "/content") }, CaptureTimeout: 3 * time.Second})
	if err != nil || request == nil || request.Request == nil {
		t.Fatalf("authenticated request capture: %+v %v", request, err)
	}
	response, err := m.CapturePoolResponse(ctx, key, s.server.URL+"/content", browser.ResponseOptions{Match: func(r browser.CapturedResponse) bool { return strings.HasSuffix(r.Request.URL, "/content") }, Timeout: 3 * time.Second})
	if err != nil || response == nil || !strings.Contains(string(response.Body), `window.firstSID="b"`) {
		t.Fatalf("authenticated response lost identity: %+v %v", response, err)
	}
}

func TestPoolFixedProxyAuthenticationFailurePreservesCredentials(t *testing.T) {
	fixedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusProxyAuthRequired) }))
	defer fixedProxy.Close()
	m, srv, _ := poolManager(t, nil, "a", "b")
	m.client = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL, fixedProxy.URL}}})
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.ProxyURL = fixedProxy.URL
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "p", "a", "b")
	a := acquirePool(t, p, key)
	_, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, ErrFixedProxyUnavailable) {
		t.Fatalf("expected fixed proxy classification: %v", err)
	}
	s, _ := p.Get(key)
	if s.CurrentID != "a" || !s.Accounts[0].Management.ProxyUnavailable || s.Accounts[0].Management.Quarantined {
		t.Fatal("proxy failure incorrectly invalidated identity")
	}
	e.mu.Lock()
	retained := e.current != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("proxy failure discarded credentials")
	}
	if available, _ := p.Readiness(key); available {
		t.Fatal("unavailable proxy accepted work")
	}
	if s.Accounts[0].Management.Reason != "fixed-proxy-unavailable" {
		t.Fatal("fixed proxy reason absent")
	}
}

func TestPoolCoordinatorCancellationReleasesAuthenticationSlot(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	entered := make(chan struct{})
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.Login = func(ctx context.Context, _ *Credentials) error { close(entered); <-ctx.Done(); return ctx.Err() }
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	createPool(t, p, "p", "a")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not begin authentication")
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel authentication")
	}
	if len(e.slots) != 0 || m.Pools() != nil {
		t.Fatal("coordinator leaked permit or lifecycle registration")
	}
}

func TestPoolEnablePersistenceFailureCannotRestoreEligibility(t *testing.T) {
	m, _, checks := poolManager(t, nil, "a")
	store := &failingManagementStore{MemoryPoolStore: NewMemoryPoolStore()}
	p := testPools(t, m, store)
	key := createPool(t, p, "p", "a")
	a := acquirePool(t, p, key)
	s, _ := p.Get(key)
	if err := p.SetDisabled(context.Background(), a.Account, true, s.Accounts[0].Management.Version); err != nil {
		t.Fatal(err)
	}
	s, _ = p.Get(key)
	store.fail.Store(true)
	before := checks.Load()
	if err := p.SetDisabled(context.Background(), a.Account, false, s.Accounts[0].Management.Version); err == nil {
		t.Fatal("failed persistence reported success")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, key, HTTP); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unsaved enable made account selectable", err)
	}
	if checks.Load() != before {
		t.Fatal("unsaved enable started verification")
	}
	store.fail.Store(false)
	got := acquirePool(t, p, key)
	if got.Account != a.Account || checks.Load() <= before {
		t.Fatal("saved enable did not verify before reuse")
	}
}

func TestPoolExplicitOperationsApplyHostContextAndRetainAttempt(t *testing.T) {
	m, srv, _ := poolManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Pool-Site") != "s" {
			t.Error("explicit pool exchange bypassed host context")
		}
		fmt.Fprint(w, "ok")
	}, "a")
	var applied atomic.Int32
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond, Context: func(ctx context.Context, key Key) context.Context {
		applied.Add(1)
		return core.WithHeaders(ctx, map[string]string{"X-Pool-Site": key.Site})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := createPool(t, p, "p", "a")
	a := acquirePool(t, p, key)
	if _, err = m.DoPool(context.Background(), key, htmlfetch.Request{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Do(WithAttempt(context.Background(), a), Key{Site: "s", ID: "different"}, htmlfetch.Request{URL: srv.URL}); !errors.Is(err, ErrAttemptChanged) {
		t.Fatal("explicit account bypassed task identity", err)
	}
	if applied.Load() < 2 {
		t.Fatal("host context was not applied to verification and exchange")
	}
}
