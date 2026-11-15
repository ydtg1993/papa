package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type sessionBridgeFixture struct {
	url         string
	client      *htmlfetch.Client
	pool        *browser.Pool
	logins      atomic.Int32
	denyBrowser atomic.Bool
}

func newSessionBridgeFixture(t *testing.T) *sessionBridgeFixture {
	t.Helper()
	f := &sessionBridgeFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/login" {
			f.logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: r.URL.Query().Get("name") + "/initial", Path: "/", HttpOnly: true})
			fmt.Fprint(w, "logged in")
			return
		}
		cookie, err := r.Cookie("sid")
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<html><body>fixture</body></html>")
			return
		}
		if r.URL.Path == "/header" {
			fmt.Fprint(w, r.Header.Get("X-Csrf-Token"))
			return
		}
		if err != nil {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/rotate" {
			cookie.Value = strings.Split(cookie.Value, "/")[0] + "/" + r.URL.Query().Get("to")
			cookie.Path = "/"
			cookie.HttpOnly = true
			http.SetCookie(w, cookie)
		}
		fmt.Fprint(w, cookie.Value)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	f.client = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	f.pool = accountBrowserPool(t, srv.URL)
	return f
}

func (f *sessionBridgeFixture) config(name string, rodLogin bool) Config {
	cfg := Config{Origins: []string{f.url}, MaxConcurrent: 4, CheckInterval: time.Hour, Timeout: 20 * time.Second}
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		r, err := f.client.Do(c.Context(ctx), htmlfetch.Request{URL: f.url + "/who", NoRedirect: true})
		if r != nil && r.StatusCode == 401 {
			return false, nil
		}
		return r != nil && strings.HasPrefix(string(r.Body), name+"/"), err
	}
	cfg.Browser = &BrowserConfig{Pool: f.pool, URL: f.url + "/", SessionBridge: true, Check: func(ctx context.Context, page *rod.Page) (bool, error) {
		if f.denyBrowser.Load() {
			return false, nil
		}
		r, err := page.Context(ctx).Eval(`async () => (await fetch('/who')).text()`)
		if err != nil {
			return false, err
		}
		return strings.HasPrefix(r.Value.Str(), name+"/"), nil
	}}
	if rodLogin {
		cfg.Browser.Login = func(ctx context.Context, page *rod.Page) error {
			if err := page.Context(ctx).Navigate(f.url + "/login?name=" + name); err != nil {
				return err
			}
			return page.WaitLoad()
		}
	} else {
		cfg.Login = func(ctx context.Context, c *Credentials) error {
			_, err := f.client.Do(c.Context(ctx), htmlfetch.Request{URL: f.url + "/login?name=" + name, NoRedirect: true})
			return err
		}
	}
	return cfg
}

func TestSessionBridgeBothLoginSourcesRotationIsolationAndReload(t *testing.T) {
	for _, rodLogin := range []bool{false, true} {
		t.Run(fmt.Sprint("rod=", rodLogin), func(t *testing.T) {
			f := newSessionBridgeFixture(t)
			m := NewManager(f.client)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			store, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{1}, 32))
			if err != nil {
				t.Fatal(err)
			}
			key := Key{Site: "s", ID: "alice"}
			cfg := f.config(key.ID, rodLogin)
			cfg.Store = store
			register(t, m, key, cfg)
			register(t, m, Key{Site: "s", ID: "bob"}, f.config("bob", rodLogin))
			if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
				_, err := page.Eval(`async () => (await fetch('/rotate?to=browser')).text()`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			r, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"})
			if err != nil || string(r.Body) != "alice/browser" {
				t.Fatalf("browser -> HTTP: %v", err)
			}
			if _, err = m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/rotate?to=http"}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"bob", "alice"} {
				want := name + "/initial"
				if name == "alice" {
					want = "alice/http"
				}
				if err = m.WithBrowser(ctx, Key{Site: "s", ID: name}, func(page *rod.Page) error {
					r, err := page.Eval(`async () => (await fetch('/who')).text()`)
					if err == nil && r.Value.Str() != want {
						return errors.New("cross-account or stale session")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if f.logins.Load() != 2 {
				t.Fatalf("login count=%d", f.logins.Load())
			}
			m2 := NewManager(f.client)
			register(t, m2, key, cfg)
			if err = m2.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if f.logins.Load() != 2 {
				t.Fatal("restart logged in again")
			}
			s := accountBrowserStatus(t, m2, key)
			if !s.HTTPReady || !s.BrowserReady || s.MaxConcurrent != 1 {
				t.Fatalf("bad readiness/budget: %+v", s)
			}
		})
	}
}

func TestSessionBridgeFailurePreservesHTTPAndLogoutWins(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	register(t, m, key, f.config("alice", false))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"}); err != nil {
		t.Fatal(err)
	}
	f.denyBrowser.Store(true)
	called := 0
	err := m.WithBrowser(ctx, key, func(*rod.Page) error { called++; return nil })
	if !errors.Is(err, ErrExpired) || called != 0 {
		t.Fatalf("unverified callback: %v / %d", err, called)
	}
	if !accountBrowserStatus(t, m, key).HTTPReady {
		t.Fatal("browser failure cleared HTTP")
	}
	f.denyBrowser.Store(false)
	err = m.WithBrowser(ctx, key, func(*rod.Page) error { called++; return m.Logout(ctx, key) })
	if !errors.Is(err, ErrSuperseded) || called != 1 {
		t.Fatalf("logout: %v / %d", err, called)
	}
	s := accountBrowserStatus(t, m, key)
	if s.HTTPReady || s.BrowserReady || !s.Paused {
		t.Fatal("stale browser restored logged-out account")
	}
}

func TestSessionBridgePoolBothNeverReloginsInAttempt(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	register(t, m, key, f.config("alice", false))
	p := testPools(t, m, NewMemoryPoolStore())
	pk := PoolKey{Site: "s", Name: "both"}
	if _, err := p.Upsert(context.Background(), PoolConfig{Key: pk, Transport: Both, Members: []string{"alice"}}, 0); err != nil {
		t.Fatal(err)
	}
	a := acquirePool(t, p, pk)
	ctx := WithAttempt(context.Background(), a)
	if err := m.WithPoolBrowser(ctx, pk, func(*rod.Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DoPool(ctx, pk, htmlfetch.Request{URL: f.url + "/who"}); err != nil {
		t.Fatal(err)
	}
	f.denyBrowser.Store(true)
	before := f.logins.Load()
	called := false
	err := m.WithPoolBrowser(ctx, pk, func(*rod.Page) error { called = true; return nil })
	if !errors.Is(err, ErrExpired) || called || f.logins.Load() != before {
		t.Fatalf("pool replay/login: %v", err)
	}
}

type bridgeFaultStore struct {
	Store
	fail atomic.Bool
}

func (s *bridgeFaultStore) Save(ctx context.Context, key Key, state *StoredState) error {
	if s.fail.Load() {
		return errors.New("synthetic disk failure")
	}
	return s.Store.Save(ctx, key, state)
}

func TestSessionBridgeSaveFailureCancellationAndOrdering(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	base, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &bridgeFaultStore{Store: base}
	cfg := f.config("alice", false)
	cfg.Store = store
	register(t, m, key, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"}); err != nil {
		t.Fatal(err)
	}
	store.fail.Store(true)
	calls := 0
	err = m.WithBrowser(ctx, key, func(page *rod.Page) error {
		calls++
		_, err := page.Eval(`async () => (await fetch('/rotate?to=unsaved')).text()`)
		return err
	})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "state-save-failed" || calls != 1 {
		t.Fatalf("save failure/replay: %v calls=%d", err, calls)
	}
	store.fail.Store(false)
	r, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"})
	if err != nil || string(r.Body) != "alice/initial" {
		t.Fatal("failed save published rotated HTTP state", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithBrowser(ctx, key, func(*rod.Page) error { close(started); <-release; return nil })
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCtx, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	_, err = m.Do(waitCtx, key, htmlfetch.Request{URL: f.url + "/who"})
	stop()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP exchange was not serialized/cancellable: %v", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	cancelCtx, stop := context.WithCancel(ctx)
	err = m.WithBrowser(cancelCtx, key, func(*rod.Page) error { stop(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if !accountBrowserStatus(t, m, key).HTTPReady {
		t.Fatal("cancel cleared HTTP readiness")
	}
}

func TestSessionBridgeDeferredPoolAndHTTPFirst(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	cfg := f.config("alice", false)
	cfg.Browser.Pool = nil
	var ready atomic.Bool
	cfg.Browser.PoolProvider = func() *browser.Pool {
		if ready.Load() {
			return f.pool
		}
		return nil
	}
	register(t, m, key, cfg)
	ctx := context.Background()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/who"}); err != nil {
		t.Fatal(err)
	}
	if err := m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if !accountBrowserStatus(t, m, key).HTTPReady {
		t.Fatal("missing browser destroyed HTTP login")
	}
	ready.Store(true)
	if err := m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.logins.Load() != 1 {
		t.Fatal("bridge logged in twice")
	}
}

func TestSessionBridgeTokenMappingAndNewCandidateWins(t *testing.T) {
	f := newSessionBridgeFixture(t)
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	cfg := f.config("alice", false)
	cfg.Browser.Import = func(_ context.Context, c *Credentials, state *browser.PageState) error {
		state.LocalStorage["csrf"] = c.Headers["X-Csrf-Token"]
		return nil
	}
	cfg.Browser.Export = func(_ context.Context, page *rod.Page, c *Credentials) error {
		r, err := page.Eval(`() => localStorage.getItem('csrf')`)
		if err == nil {
			c.Headers["X-Csrf-Token"] = r.Value.Str()
		}
		return err
	}
	register(t, m, key, cfg)
	ctx := context.Background()
	if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		_, err := page.Eval(`() => localStorage.setItem('csrf','rotated-token')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/header"})
	if err != nil || string(r.Body) != "rotated-token" {
		t.Fatal("browser token export failed", err)
	}
	err = m.WithBrowser(ctx, key, func(page *rod.Page) error {
		r, err := page.Eval(`() => localStorage.getItem('csrf')`)
		if err != nil {
			return err
		}
		if r.Value.Str() != "rotated-token" {
			return errors.New("HTTP token import failed")
		}
		e, _ := m.get(key)
		e.mu.Lock()
		candidate, err := detachedCandidate(e.current)
		version := e.version
		e.mu.Unlock()
		if err != nil {
			return err
		}
		candidate.BrowserState = nil
		candidate.Headers["X-Csrf-Token"] = "new-publication"
		_, err = m.PublishCandidate(ctx, key, version, candidate)
		return err
	})
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old browser overwrote new candidate: %v", err)
	}
	r, err = m.Do(ctx, key, htmlfetch.Request{URL: f.url + "/header"})
	if err != nil || string(r.Body) != "new-publication" {
		t.Fatal("new candidate lost", err)
	}
}

func TestSessionBridgeBrowserOnlyCandidateNeedsNoHTTPAdapter(t *testing.T) {
	f := newSessionBridgeFixture(t)
	cfg := f.config("alice", true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var state *browser.PageState
	err := f.pool.WithIsolatedBrowser(ctx, func(b *browser.Browser) error {
		return b.WithStatePage(ctx, f.url+"/", nil, browser.PageOptions{}, func(page *rod.Page) error {
			if err := cfg.Browser.Login(ctx, page); err != nil {
				return err
			}
			var err error
			state, err = browser.ExportPageState(page)
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := &Credentials{BrowserState: state} // Provider returns only its Rod login result.
	m := NewManager(f.client)
	key := Key{Site: "s", ID: "alice"}
	if err = m.RegisterCandidate(ctx, key, cfg, candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Session != nil {
		t.Fatal("framework mutated provider's candidate")
	}
	if _, err = m.FetchHTML(ctx, key, f.url+"/who"); err != nil {
		t.Fatal(err)
	}
	if err = m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s := accountBrowserStatus(t, m, key)
	if !s.HTTPReady || !s.BrowserReady || f.logins.Load() != 1 {
		t.Fatal("candidate bridge did not verify both transports without re-login")
	}
}
