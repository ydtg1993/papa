package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func bridgeCookie(t *testing.T, e *entry, rawURL string) string {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "<missing>"
	}
	u, _ := url.Parse(rawURL)
	for _, c := range e.current.Session.Cookies(u) {
		if c.Name == "sid" {
			return c.Value
		}
	}
	return ""
}

func TestSessionBridgeReviewHealthUsesLatestCookiesAndKeepsRotation(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	cfg := f.config("alice", false)
	base, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &bridgeFaultStore{Store: base}
	cfg.Store = store
	var probing atomic.Bool
	var rotations atomic.Int32
	check := cfg.Check
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		if probing.Load() {
			_, err := f.client.Do(c.Context(ctx), htmlfetch.Request{URL: fmt.Sprintf("%s/rotate?to=health-%d", f.url, rotations.Add(1))})
			if err != nil {
				return false, err
			}
		}
		return check(ctx, c)
	}
	var seen string
	bcheck := cfg.Browser.Check
	cfg.Browser.Check = func(ctx context.Context, page *rod.Page) (bool, error) {
		r, err := page.Context(ctx).Eval(`async () => (await fetch('/who')).text()`)
		if err != nil {
			return false, err
		}
		seen = r.Value.Str()
		return bcheck(ctx, page)
	}
	register(t, m, key, cfg)
	ctx := context.Background()
	if err := m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/rotate?to=http"}); err != nil {
		t.Fatal(err)
	}
	p := &Pools{manager: m}
	probing.Store(true)
	if _, _, err := p.checkTransport(ctx, key, Both, 0, 0); err != nil {
		t.Fatal(err)
	}
	if seen != "alice/health-1" {
		t.Errorf("health browser restored stale cookie: %q", seen)
	}
	e, _ := m.get(key)
	if got := bridgeCookie(t, e, f.url); got != "alice/health-2" {
		t.Errorf("health check discarded rotation: %q", got)
	}
	saved, err := base.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Browser == nil || len(saved.Browser.Cookies) != 1 || saved.Browser.Cookies[0].Value != "alice/health-2" {
		t.Fatal("persisted browser snapshot missed post-browser HTTP rotation")
	}
	store.fail.Store(true)
	if _, _, err = p.checkTransport(ctx, key, HTTP, 0, 0); err == nil {
		t.Fatal("health ignored save error")
	}
	if got := bridgeCookie(t, e, f.url); got != "alice/health-2" {
		t.Fatalf("failed health save replaced state: %s", got)
	}
}

func TestSessionBridgeReviewForbiddenResponseStillAdvancesCookieState(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	register(t, m, key, f.config("alice", false))
	ctx := context.Background()
	if err := m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	e, _ := m.get(key)
	e.mu.Lock()
	before := e.browserRevision
	e.mu.Unlock()
	_, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/forbidden-rotate?to=denied"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browserRevision == before || e.pageState.Cookies[0].Value != "alice/denied" {
		t.Fatal("classified failure left stale cookie evidence")
	}
}

func TestSessionBridgeReviewPreflightFailureRetainsCommittedState(t *testing.T) {
	for _, mode := range []string{"check-error", "check-cancel", "check-panic", "save-error", "save-panic"} {
		t.Run(mode, func(t *testing.T) {
			f := newSessionBridgeFixture(t)
			m := NewManager(f.client)
			key := Key{Site: "s", ID: "alice"}
			base, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{8}, 32))
			if err != nil {
				t.Fatal(err)
			}
			store := &bridgeReviewStore{Store: base}
			cfg := f.config("alice", false)
			cfg.Store = store
			var fail atomic.Bool
			check := cfg.Check
			cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
				if fail.Load() {
					_, err := f.client.Do(c.Context(ctx), htmlfetch.Request{URL: f.url + "/rotate?to=uncommitted"})
					if err != nil {
						return false, err
					}
					if mode == "check-error" {
						return false, errors.New("synthetic check failure")
					}
					if mode == "check-cancel" {
						return false, context.Canceled
					}
					if mode == "check-panic" {
						panic("synthetic check panic")
					}
				}
				return check(ctx, c)
			}
			register(t, m, key, cfg)
			ctx := context.Background()
			if err = m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
				t.Fatal(err)
			}
			e, _ := m.get(key)
			e.mu.Lock()
			e.lastCheck = time.Time{}
			e.mu.Unlock()
			fail.Store(true)
			store.mode = mode
			func() {
				defer func() { _ = recover() }()
				err = m.WithBrowser(ctx, key, func(*rod.Page) error { t.Error("callback after failed preflight"); return nil })
			}()
			if got := bridgeCookie(t, e, f.url); got != "alice/initial" {
				t.Errorf("failed preflight changed committed state: %q", got)
			}
			e.mu.Lock()
			hasBrowser := e.pageState != nil
			e.mu.Unlock()
			if !hasBrowser {
				t.Error("failed preflight discarded browser state")
			}
		})
	}
}

type bridgeReviewStore struct {
	Store
	mode string
}

func TestSessionBridgeReviewPoolFailedCandidatePreservesBothTransports(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	cfg := f.config("alice", false)
	var denyHTTP atomic.Bool
	check := cfg.Check
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		if denyHTTP.Load() {
			return false, nil
		}
		return check(ctx, c)
	}
	register(t, m, key, cfg)
	p := testPools(t, m, NewMemoryPoolStore())
	pk := PoolKey{Site: "s", Name: "both"}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: pk, Transport: Both, Members: []string{"alice"}}, 0); err != nil {
		t.Fatal(err)
	}
	a := acquirePool(t, p, pk)
	err := m.WithPoolBrowser(WithAttempt(context.Background(), a), pk, func(*rod.Page) error { denyHTTP.Store(true); return nil })
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected failed post-browser HTTP check: %v", err)
	}
	e, _ := m.get(key)
	e.mu.Lock()
	retained := e.current != nil && e.pageState != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("failed detached candidate deleted committed credentials")
	}
	p.mu.Lock()
	state := p.accounts[key].state
	p.mu.Unlock()
	if state.CheckTransport != HTTP || !state.Suspicious || !state.VerificationPending {
		t.Fatalf("candidate failure did not request HTTP verification: transport=%s pending=%v", state.CheckTransport, state.VerificationPending)
	}
}

func TestSessionBridgeReviewHealthSettlementPreservesOtherOrNewerState(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint("stale=", stale), func(t *testing.T) {
			f := newSessionBridgeFixture(t)
			m := NewManager(f.client)
			key := Key{Site: "s", ID: "alice"}
			register(t, m, key, f.config("alice", false))
			p := testPools(t, m, NewMemoryPoolStore())
			pk := PoolKey{Site: "s", Name: "both"}
			ctx := context.Background()
			if _, err := p.Upsert(ctx, PoolConfig{Key: pk, Transport: Both, Members: []string{"alice"}}, 0); err != nil {
				t.Fatal(err)
			}
			_ = acquirePool(t, p, pk)
			p.mu.Lock()
			a := p.accounts[key]
			a.verifying = true
			a.state.Policy.InvalidConfirmations = 1
			before := cloneManagement(a.state)
			p.mu.Unlock()
			f.denyBrowser.Store(true)
			v, bv, err := p.checkTransport(ctx, key, Both, 0, 0)
			if !errors.Is(err, ErrExpired) {
				t.Fatal(err)
			}
			if stale {
				if _, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/rotate?to=newer"}); err != nil {
					t.Fatal(err)
				}
			}
			p.finishVerification(key, Both, before, CheckExisting, v, bv, err, time.Minute)
			e, _ := m.get(key)
			e.mu.Lock()
			hasHTTP, hasBrowser := e.current != nil, e.pageState != nil
			e.mu.Unlock()
			if !hasHTTP || hasBrowser != stale {
				t.Fatalf("health cleared other/newer state: HTTP=%v browser=%v stale=%v", hasHTTP, hasBrowser, stale)
			}
			if stale && bridgeCookie(t, e, f.url) != "alice/newer" {
				t.Fatal("old health result replaced new cookies")
			}
		})
	}
}

func (s *bridgeReviewStore) Save(ctx context.Context, key Key, state *StoredState) error {
	if s.mode == "save-panic" {
		panic("synthetic save panic")
	}
	if s.mode == "save-error" {
		return errors.New("synthetic save failure")
	}
	return s.Store.Save(ctx, key, state)
}

func TestSessionBridgeReviewCandidateWaitsAndRejectsStaleCookie(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	cfg := f.config("alice", false)
	var verify atomic.Bool
	check := cfg.Check
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		if verify.Load() {
			t.Error("candidate verification overlapped browser exchange")
		}
		return check(ctx, c)
	}
	register(t, m, key, cfg)
	ctx := context.Background()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"}); err != nil {
		t.Fatal(err)
	}
	e, _ := m.get(key)
	e.mu.Lock()
	c, err := detachedCandidate(e.current)
	v := e.version
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithBrowser(ctx, key, func(page *rod.Page) error {
			close(started)
			<-release
			_, err := page.Eval(`async () => (await fetch('/rotate?to=new')).text()`)
			return err
		})
	}()
	<-started
	verify.Store(true)
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	_, err = m.PublishCandidate(waitCtx, key, v, c)
	cancel()
	verify.Store(false)
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("candidate did not wait for exchange: %v", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got := bridgeCookie(t, e, f.url); got != "alice/new" {
		t.Errorf("stale publication replaced rotation: %q", got)
	}
}
