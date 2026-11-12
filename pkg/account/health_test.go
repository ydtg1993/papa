package account

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func fastHealthPolicy() Policy {
	v := DefaultPolicy()
	v.Buffer = 25 * time.Millisecond
	v.CheckInterval = 35 * time.Millisecond
	v.UnknownBackoff = 25 * time.Millisecond
	v.MaxBackoff = 100 * time.Millisecond
	return v
}
func setHealthPolicy(t *testing.T, p *Pools, key Key, v Policy) {
	t.Helper()
	_, rev, err := p.Policy(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SetPolicy(context.Background(), key, v, rev); err != nil {
		t.Fatal(err)
	}
}
func managementFor(t *testing.T, p *Pools, pool PoolKey, key Key) ManagementState {
	t.Helper()
	s, err := p.Get(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Accounts {
		if a.Key == key {
			return a.Management
		}
	}
	t.Fatal("missing member")
	return ManagementState{}
}

func TestDefault401NeedsVerifyAndExplicitExpiry(t *testing.T) {
	if defaultClassify(&htmlfetch.Response{StatusCode: 401}, nil) != NeedsVerify {
		t.Fatal("401 is not suspicious")
	}
	if !IsAttemptFailure(ErrNeedsVerify) || !IsAttemptFailure(ErrBudget) {
		t.Fatal("attempt gate errors not classified")
	}
}

func TestSuspicious401ChecksExistingWithoutLoginOrReplay(t *testing.T) {
	var calls, logins atomic.Int32
	m, srv, _ := poolManager(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) }, "a", "b")
	e, _ := m.get(Key{Site: "s", ID: "a"})
	login := e.config.Login
	e.config.Login = func(ctx context.Context, c *Credentials) error { logins.Add(1); return login(ctx, c) }
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	_, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL})
	if !errors.Is(err, ErrNeedsVerify) || calls.Load() != 1 {
		t.Fatalf("response=%v calls=%d", err, calls.Load())
	}
	s, _ := p.Get(key)
	if s.CurrentID != "a" || !s.Accounts[0].Management.Suspicious || s.Accounts[0].Management.Quarantined {
		t.Fatalf("unexpected suspicion: %+v", s)
	}
	e.mu.Lock()
	retained := e.current != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("lost credentials")
	}
	if _, err = m.Do(context.Background(), a.Account, htmlfetch.Request{URL: srv.URL}); !errors.Is(err, ErrNeedsVerify) {
		t.Fatalf("explicit API bypassed gate: %v", err)
	}
	next := acquirePool(t, p, key)
	if next.Account != a.Account || next.SelectionVersion != a.SelectionVersion || logins.Load() != 1 {
		t.Fatalf("recovery changed identity or logged in: %+v logins=%d", next, logins.Load())
	}
}

func TestSuspiciousConcurrentReportsRequireSeparatedInvalidChecks(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	var invalid atomic.Bool
	var logins atomic.Int32
	e, _ := m.get(Key{Site: "s", ID: "a"})
	login := e.config.Login
	e.config.Login = func(ctx context.Context, c *Credentials) error { logins.Add(1); return login(ctx, c) }
	var mu sync.Mutex
	var times []time.Time
	e.config.Check = func(context.Context, *Credentials) (bool, error) {
		if invalid.Load() {
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
			return false, nil
		}
		return true, nil
	}
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	policy := fastHealthPolicy()
	setHealthPolicy(t, p, a.Account, policy)
	invalid.Store(true)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.ReportFailure(a, NeedsVerify, 99) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("coalesced %d reports", won.Load())
	}
	eventuallyPool(t, func() bool { return managementFor(t, p, key, a.Account).InvalidChecks == 1 })
	s, _ := p.Get(key)
	if s.CurrentID != "a" || s.Accounts[0].Management.Quarantined {
		t.Fatal("first independent check isolated account")
	}
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("confirmed invalid account not replaced")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(times[0]) < policy.CheckInterval {
		t.Fatalf("checks not separated: %v", times)
	}
	if logins.Load() != 1 {
		t.Fatal("existing-state check logged in")
	}
}

func TestUnknownVerificationBacksOffWithoutInvalidEvidence(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	var unknown atomic.Bool
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.Check = func(context.Context, *Credentials) (bool, error) {
		if unknown.Load() {
			return false, errors.New("secret-provider-token")
		}
		return true, nil
	}
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	unknown.Store(true)
	p.ReportFailure(a, NeedsVerify, 0)
	eventuallyPool(t, func() bool { return managementFor(t, p, key, a.Account).UnknownChecks >= 2 })
	s := managementFor(t, p, key, a.Account)
	if s.InvalidChecks != 0 || s.Quarantined || !s.Suspicious || !s.NextVerifyAt.After(time.Now()) {
		t.Fatalf("unknown treated as invalid: %+v", s)
	}
	for _, event := range p.Events(a.Account, 0) {
		if event.Reason == "secret-provider-token" {
			t.Fatal("provider secret leaked")
		}
	}
	unknown.Store(false)
	next := acquirePool(t, p, key)
	if next.Account != a.Account {
		t.Fatal("unknown response rotated account")
	}
}

func TestCheckExistingNeverResumesPausedAccount(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	if err := m.Pause(a.Account); err != nil {
		t.Fatal(err)
	}
	_, version, _ := p.Policy(a.Account)
	if err := p.CheckExisting(context.Background(), a.Account, version); err != nil {
		t.Fatal(err)
	}
	eventuallyPool(t, func() bool { return managementFor(t, p, key, a.Account).UnknownChecks > 0 })
	if !m.Status()[0].Paused {
		t.Fatal("check implicitly resumed paused account")
	}
}

func TestCheckExistingStaleResultCannotInvalidateNewCandidate(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	e, _ := m.get(Key{Site: "s", ID: "a"})
	var suspect atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.config.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		if suspect.Load() && c.Headers["Authorization"] != "fresh" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		return true, nil
	}
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	suspect.Store(true)
	p.ReportFailure(a, NeedsVerify, 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("check did not begin")
	}
	c := &Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{"Authorization": "fresh"}}
	if _, err := m.PublishCandidate(context.Background(), a.Account, a.CredentialVersion, c); err != nil {
		t.Fatal(err)
	}
	close(release)
	next := acquirePool(t, p, key)
	if next.Account != a.Account || next.CredentialVersion == a.CredentialVersion {
		t.Fatalf("new candidate lost: %+v", next)
	}
	if p.ReportFailure(a, Expired, 0) {
		t.Fatal("late old response invalidated candidate")
	}
}

func TestSuspiciousRestartRetainsStateAndChecksWithoutLogin(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	e, _ := m.get(Key{Site: "s", ID: "a"})
	fs, err := NewFileStore(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.config.Store = fs
	store := NewMemoryPoolStore()
	p, err := NewPools(context.Background(), m, store, PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	v := fastHealthPolicy()
	v.Buffer = 150 * time.Millisecond
	setHealthPolicy(t, p, a.Account, v)
	p.ReportFailure(a, NeedsVerify, 0)
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Accounts[0].Persisted })
	p.Close()
	m2 := NewManager(m.client)
	cfg := e.config
	var logins atomic.Int32
	cfg.Login = func(context.Context, *Credentials) error { logins.Add(1); return errors.New("unexpected login") }
	if err = m2.Register(a.Account, cfg); err != nil {
		t.Fatal(err)
	}
	p2 := testPools(t, m2, store)
	s, _ := p2.Get(key)
	if !s.Accounts[0].Management.Suspicious || s.CurrentID != "a" {
		t.Fatal("restart cleared pending suspicion")
	}
	next := acquirePool(t, p2, key)
	if next.Account != a.Account || logins.Load() != 0 {
		t.Fatal("restart logged in or rotated")
	}
}

func TestAccountUsageBudgetSharedAcrossPoolsAndPersisted(t *testing.T) {
	m, srv, _ := poolManager(t, nil, "a", "b")
	store := NewMemoryPoolStore()
	p := testPools(t, m, store)
	one := createPool(t, p, "one", "a", "b")
	two := createPool(t, p, "two", "a", "b")
	a := acquirePool(t, p, one)
	b := acquirePool(t, p, two)
	v := fastHealthPolicy()
	v.DailyLimit = 2
	setHealthPolicy(t, p, a.Account, v)
	for _, attempt := range []*Attempt{a, b} {
		if _, err := m.DoPool(WithAttempt(context.Background(), attempt), attempt.Pool, htmlfetch.Request{URL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.DoPool(WithAttempt(context.Background(), a), one, htmlfetch.Request{URL: srv.URL}); !errors.Is(err, ErrBudget) {
		t.Fatalf("quota not shared: %v", err)
	}
	for _, key := range []PoolKey{one, two} {
		s, _ := p.Get(key)
		if s.CurrentID != "a" || s.WaitReason != "current-account-daily-budget-exhausted" {
			t.Fatalf("quota rotated identity: %+v", s)
		}
	}
	rows, _ := store.LoadManagement(context.Background())
	for _, s := range rows {
		if s.Key == a.Account && (s.Usage.Unit != "operation" || s.Usage.Operations != 2) {
			t.Fatalf("usage not persisted: %+v", s)
		}
	}
}

func TestAccountUsageWindowsAndLoginInterval(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
	v := DefaultPolicy()
	v.WorkStartMinute = 22 * 60
	v.WorkEndMinute = 6 * 60
	if usageReason(ManagementState{Policy: v}, now) != "" || usageReason(ManagementState{Policy: v}, now.Add(12*time.Hour)) != "outside-work-hours" {
		t.Fatal("overnight work window incorrect")
	}
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	v = DefaultPolicy()
	v.LoginInterval = time.Hour
	setHealthPolicy(t, p, a.Account, v)
	if err := m.ReserveLogin(context.Background(), a.Account); !errors.Is(err, ErrBudget) {
		t.Fatalf("last login ignored: %v", err)
	}
}

func TestStandbyVerificationOptInKeepsCurrentAccount(t *testing.T) {
	m, _, checks := poolManager(t, nil, "a", "b", "c")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b", "c")
	a := acquirePool(t, p, key)
	if checks.Load() != 1 {
		t.Fatal("standbys verified by default")
	}
	s, _ := p.Get(key)
	cfg := s.PoolConfig
	cfg.StandbyTarget = 1
	cfg.StandbyTTL = time.Hour
	if _, err := p.Upsert(context.Background(), cfg, s.Revision); err != nil {
		t.Fatal(err)
	}
	eventuallyPool(t, func() bool { s, _ := p.Get(key); return s.Available >= 2 })
	next := acquirePool(t, p, key)
	if next.Account != a.Account || checks.Load() != 2 {
		t.Fatalf("standby changed current or overfilled: checks=%d", checks.Load())
	}
}

func TestHealthEventsBoundedAndSlowListenerDoesNotBlock(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	blocked := make(chan struct{})
	defer close(blocked)
	remove := p.AddEventListener(func(HealthEvent) { <-blocked })
	defer remove()
	p.mu.Lock()
	state := p.accounts[a.Account]
	for i := 0; i < 150; i++ {
		p.eventLocked(state, "sample", "sanitized")
	}
	p.mu.Unlock()
	events := p.Events(Key{}, 0)
	if len(events) != 100 || events[len(events)-1].Sequence <= events[0].Sequence {
		t.Fatal("event history not bounded/ordered")
	}
	if _, err := p.Get(key); err != nil {
		t.Fatal(err)
	}
}

func TestHealthShutdownTimeoutRetainsDependenciesAndDiscardsLateCheck(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	e, _ := m.get(Key{Site: "s", ID: "a"})
	var block atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	e.config.Check = func(context.Context, *Credentials) (bool, error) {
		if block.Load() {
			close(entered)
			<-release
		}
		return true, nil
	}
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	block.Store(true)
	p.ReportFailure(a, NeedsVerify, 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("check not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if m.Pools() != p {
		t.Fatal("timed out shutdown detached live dependencies")
	}
	close(release)
	if err = p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !managementFor(t, p, key, a.Account).Suspicious {
		t.Fatal("late check published after shutdown")
	}
	if m.Pools() != nil {
		t.Fatal("drained coordinator not detached")
	}
}

func TestSlowSiteDoesNotBlockAnotherSiteVerification(t *testing.T) {
	m, srv, _ := poolManager(t, nil, "a")
	entered := make(chan struct{})
	release := make(chan struct{})
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.Login = func(context.Context, *Credentials) error { close(entered); <-release; return nil }
	if err := m.Register(Key{Site: "z", ID: "other"}, Config{Origins: []string{srv.URL}, Login: func(context.Context, *Credentials) error { return nil }, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	p := testPools(t, m, NewMemoryPoolStore())
	createPool(t, p, "one", "a")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow site not started")
	}
	defer close(release)
	key := PoolKey{Site: "z", Name: "other"}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: key, Members: []string{"other"}}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.Acquire(ctx, key, HTTP); err != nil {
		t.Fatalf("slow site blocked unrelated account: %v", err)
	}
}

func TestAutomaticRecoveryUsesLoginButNeverResumesManualPause(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	var logins atomic.Int32
	e, _ := m.get(Key{Site: "s", ID: "a"})
	original := e.config.Login
	e.config.Login = func(ctx context.Context, c *Credentials) error { logins.Add(1); return original(ctx, c) }
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	v := fastHealthPolicy()
	v.RecoveryInterval = 20 * time.Millisecond
	setHealthPolicy(t, p, a.Account, v)
	p.ReportFailure(a, Expired, 0)
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("strong expiry did not fail over")
	}
	eventuallyPool(t, func() bool { return logins.Load() == 2 && !managementFor(t, p, key, a.Account).Quarantined })
	if current := acquirePool(t, p, key); current.Account != b.Account {
		t.Fatal("recovered account stole healthy selection")
	}
}

func TestConfirmedProxyFailureSwitchesWithoutDiscardingCredentials(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a", "b")
	var broken atomic.Bool
	e, _ := m.get(Key{Site: "s", ID: "a"})
	e.config.Check = func(context.Context, *Credentials) (bool, error) {
		if broken.Load() {
			return false, ErrFixedProxyUnavailable
		}
		return true, nil
	}
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a", "b")
	a := acquirePool(t, p, key)
	setHealthPolicy(t, p, a.Account, fastHealthPolicy())
	broken.Store(true)
	p.ReportFailure(a, ProxyUnavailable, 0)
	b := acquirePool(t, p, key)
	if b.Account.ID != "b" {
		t.Fatal("confirmed outlet outage not replaced")
	}
	s := managementFor(t, p, key, a.Account)
	if s.InvalidChecks != 0 || s.ProxyChecks != 2 || !s.ProxyUnavailable {
		t.Fatalf("proxy failure conflated credentials: %+v", s)
	}
	e.mu.Lock()
	retained := e.current != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("proxy confirmation deleted credentials")
	}
}

func TestDefaultPolicyAddsNoPerOperationManagementWrites(t *testing.T) {
	m, srv, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	before := managementFor(t, p, key, a.Account).Version
	for i := 0; i < 5; i++ {
		if _, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	after := managementFor(t, p, key, a.Account)
	if after.Version != before || after.Usage.Operations != 0 {
		t.Fatalf("default operation mutated persistent management: before=%d after=%+v", before, after)
	}
	v := DefaultPolicy()
	v.TrackUsage = true
	setHealthPolicy(t, p, a.Account, v)
	if _, err := m.DoPool(WithAttempt(context.Background(), a), key, htmlfetch.Request{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if managementFor(t, p, key, a.Account).Usage.Operations != 1 {
		t.Fatal("opt-in usage counter not recorded")
	}
}
