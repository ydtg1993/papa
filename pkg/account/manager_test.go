package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type authSite struct {
	server                   *httptest.Server
	client                   *htmlfetch.Client
	mu                       sync.Mutex
	versions                 map[string]int
	logins, checks, writes   int
	lateStarted, lateRelease chan struct{}
	lateOnce                 sync.Once
}

func newAuthSite(t *testing.T) *authSite {
	t.Helper()
	s := &authSite{versions: map[string]int{}, lateStarted: make(chan struct{}), lateRelease: make(chan struct{})}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			// Provider contexts must not inherit another account's credentials.
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
				t.Error("login inherited credentials")
			}
			name := r.URL.Query().Get("name")
			s.mu.Lock()
			s.versions[name]++
			s.logins++
			sid := fmt.Sprintf("%s:%d", name, s.versions[name])
			s.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/", HttpOnly: true})
			fmt.Fprintf(w, `{"token":%q,"csrf":%q}`, "token-"+sid, "csrf-"+sid)
			return
		}
		cookie, err := r.Cookie("sid")
		sid := ""
		if err == nil {
			sid = cookie.Value
		}
		name := strings.Split(sid, ":")[0]
		s.mu.Lock()
		valid := sid == fmt.Sprintf("%s:%d", name, s.versions[name]) && name != ""
		if r.URL.Path == "/me" {
			s.checks++
		}
		if r.URL.Path == "/write" {
			s.writes++
		}
		s.mu.Unlock()
		if r.URL.Path == "/late" && sid == "alice:1" {
			s.lateOnce.Do(func() { close(s.lateStarted) })
			select {
			case <-s.lateRelease:
			case <-r.Context().Done():
				return
			}
			valid = false
		}
		if !valid {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-"+sid || r.Header.Get("X-Csrf-Token") != "csrf-"+sid {
			t.Error("token/CSRF not bound to cookie")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/forbidden":
			w.WriteHeader(403)
		case "/challenge":
			_, _ = io.WriteString(w, "verify-human")
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<h1>%s</h1>", name)
		case "/redirect":
			http.Redirect(w, r, "/data", 302)
		default:
			fmt.Fprintf(w, `{"account":%q}`, name)
		}
	}))
	t.Cleanup(s.server.Close)
	s.client = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{s.server.URL}}})
	return s
}
func (s *authSite) cfg(name string) Config {
	return Config{Origins: []string{s.server.URL}, CheckInterval: time.Hour, MaxConcurrent: 8, Login: func(ctx context.Context, c *Credentials) error {
		r, err := s.client.Do(c.Context(ctx), htmlfetch.Request{URL: s.server.URL + "/login?name=" + name, Method: "POST", NoRedirect: true})
		if err != nil {
			return err
		}
		var v struct{ Token, CSRF string }
		if err = r.DecodeJSON(&v); err != nil {
			return err
		}
		c.Headers["Authorization"] = "Bearer " + v.Token
		c.Headers["X-CSRF-Token"] = v.CSRF
		c.Values["refresh"] = "private-refresh"
		return nil
	}, Check: func(ctx context.Context, c *Credentials) (bool, error) {
		r, err := s.client.Do(c.Context(ctx), htmlfetch.Request{URL: s.server.URL + "/me", NoRedirect: true})
		if r != nil && r.StatusCode == 401 {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var v struct{ Account string }
		if err = r.DecodeJSON(&v); err != nil {
			return false, err
		}
		return v.Account == name, nil
	}}
}
func (s *authSite) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins, s.checks, s.writes
}
func (s *authSite) expire(name string) { s.mu.Lock(); s.versions[name]++; s.mu.Unlock() }
func register(t *testing.T, m *Manager, key Key, cfg Config) {
	t.Helper()
	if err := m.Register(key, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAccountHTTPIsolationAndCheckCache(t *testing.T) {
	s := newAuthSite(t)
	m := NewManager(s.client)
	alice := Key{"site", "alice"}
	bob := Key{"site", "bob"}
	register(t, m, alice, s.cfg("alice"))
	register(t, m, bob, s.cfg("bob"))
	ctx := core.WithHeaders(context.Background(), map[string]string{"cookie": "sid=foreign", "authorization": "foreign", "proxy-authorization": "foreign"})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := alice
			if i%2 == 1 {
				key = bob
			}
			r, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"})
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(string(r.Body), key.ID) {
				t.Errorf("wrong account: %s", r.Body)
			}
		}(i)
	}
	wg.Wait()
	logins, checks, _ := s.counts()
	if logins != 2 || checks != 2 {
		t.Fatalf("login/check=%d/%d", logins, checks)
	}
	p, err := m.FetchHTML(ctx, alice, s.server.URL+"/html")
	if err != nil || p.Selection().Find("h1").Text() != "alice" {
		t.Fatal(p, err)
	}
	if _, err = m.Do(ctx, alice, htmlfetch.Request{URL: s.server.URL + "/redirect"}); err == nil {
		t.Fatal("redirect followed")
	}
	if _, err = m.Do(ctx, alice, htmlfetch.Request{URL: s.server.URL, Headers: map[string]string{"authorization": "foreign"}}); err == nil {
		t.Fatal("credential override accepted")
	}
	if _, err = m.Do(ctx, alice, htmlfetch.Request{URL: "https://unregistered.invalid"}); err == nil {
		t.Fatal("origin escape")
	}
	if _, err = m.Do(core.WithProxyURL(ctx, "http://127.0.0.1:9"), alice, htmlfetch.Request{URL: s.server.URL}); err == nil {
		t.Fatal("proxy conflict accepted")
	}
}

func TestAccountExpiryRetryAndNoPostReplay(t *testing.T) {
	s := newAuthSite(t)
	m := NewManager(s.client)
	key := Key{"site", "alice"}
	register(t, m, key, s.cfg("alice"))
	ctx := context.Background()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"}); err != nil {
		t.Fatal(err)
	}
	old := m.Status()[0].Version
	s.expire("alice")
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"}); err != nil {
		t.Fatal(err)
	}
	if m.Invalidate(key, old) {
		t.Fatal("old response invalidated new credentials")
	}
	if n, _, _ := s.counts(); n != 2 {
		t.Fatal(n)
	}
	s.expire("alice")
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/write", Method: "POST", Body: []byte("submit")}); !errors.Is(err, ErrNeedsVerify) {
		t.Fatal(err)
	}
	if n, _, writes := s.counts(); n != 2 || writes != 1 {
		t.Fatalf("unsafe replay/login: %d %d", n, writes)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/forbidden"}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if n, _, _ := s.counts(); n != 3 {
		t.Fatal("403 refreshed credentials", n)
	}
}

func TestAccountLateExpiredResponseKeepsFreshGeneration(t *testing.T) {
	s := newAuthSite(t)
	m := NewManager(s.client)
	key := Key{"site", "alice"}
	register(t, m, key, s.cfg("alice"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/late"}); done <- err }()
	select {
	case <-s.lateStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s.expire("alice")
	_, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/data"})
	close(s.lateRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.counts(); n != 2 {
		t.Fatal("late response caused extra login", n)
	}
}

func TestAccountPauseChallengeCooldownAndProviderValidation(t *testing.T) {
	s := newAuthSite(t)
	m := NewManager(s.client)
	key := Key{"site", "alice"}
	cfg := s.cfg("alice")
	cfg.Classify = func(r *htmlfetch.Response, e error) Outcome {
		if r != nil && string(r.Body) == "verify-human" {
			return Challenge
		}
		return defaultClassify(r, e)
	}
	register(t, m, key, cfg)
	ctx := context.Background()
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/challenge"}); !errors.Is(err, ErrChallenge) {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	if err := m.Resume(key); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(key); err != nil {
		t.Fatal(err)
	}
	if !m.Status()[0].Paused {
		t.Fatal("pause not visible")
	}
	var attempts atomic.Int32
	bad := Key{"site", "bad"}
	cfg = s.cfg("bad")
	cfg.Login = func(context.Context, *Credentials) error {
		attempts.Add(1)
		return errors.New("secret token must not appear in status")
	}
	register(t, m, bad, cfg)
	for i := 0; i < 2; i++ {
		_, err := m.Do(ctx, bad, htmlfetch.Request{URL: s.server.URL})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatal("cooldown failed")
	}
	unsafe := Key{"site", "unsafe"}
	cfg = s.cfg("unsafe")
	cfg.Check = func(_ context.Context, c *Credentials) (bool, error) {
		c.Headers["Cookie"] = "foreign"
		return true, nil
	}
	register(t, m, unsafe, cfg)
	if _, err := m.Do(ctx, unsafe, htmlfetch.Request{URL: s.server.URL}); err == nil {
		t.Fatal("provider bypassed credential header validation")
	}
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestAccountCancelledLeaderAndPanicRelease(t *testing.T) {
	for _, mode := range []string{"cancel", "panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
			key := Key{"site", mode}
			started, release := make(chan struct{}), make(chan struct{})
			cfg := Config{Origins: []string{"https://example.com"}, MaxConcurrent: 2, Login: func(ctx context.Context, c *Credentials) error {
				close(started)
				<-release
				switch mode {
				case "panic":
					panic("provider panic")
				case "goexit":
					runtime.Goexit()
				}
				return ctx.Err()
			}, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }}
			register(t, m, key, cfg)
			e, _ := m.get(key)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			leader := make(chan struct{})
			go func() { defer close(leader); defer func() { _ = recover() }(); _, _, _ = e.ensure(ctx) }()
			<-started
			waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			waiter := make(chan error, 1)
			observed := &observedContext{Context: waitCtx, entered: make(chan struct{})}
			go func() { _, _, err := e.ensure(observed); waiter <- err }()
			<-observed.entered
			if mode == "cancel" {
				cancel()
			}
			close(release)
			<-leader
			if err := <-waiter; err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("waiter was not released", err)
			}
			e.mu.Lock()
			busy := e.flight != nil
			e.mu.Unlock()
			if busy {
				t.Fatal("flight leaked")
			}
		})
	}
}

func TestAccountTokenRotationAndCheckErrors(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		want := "Bearer old"
		if n > 1 {
			want = "Bearer rotated"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("rotated token not used")
		}
		w.Header().Set("X-New-Token", "rotated")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	m := NewManager(htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}))
	key := Key{"site", "token"}
	var logins atomic.Int32
	cfg := Config{Origins: []string{" " + srv.URL + " "}, CheckInterval: time.Hour, Login: func(_ context.Context, c *Credentials) error {
		logins.Add(1)
		c.Headers["Authorization"] = "Bearer old"
		return nil
	}, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }, Update: func(c *Credentials, r *htmlfetch.Response) error {
		c.Headers["Authorization"] = "Bearer " + r.Header.Get("X-New-Token")
		c.Values["csrf"] = "updated"
		return nil
	}}
	register(t, m, key, cfg)
	if _, err := m.Do(context.Background(), key, htmlfetch.Request{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(context.Background(), key, htmlfetch.Request{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 1 {
		t.Fatal("rotation caused relogin")
	}
	e, _ := m.get(key)
	e.mu.Lock()
	e.lastCheck = time.Time{}
	e.config.Check = func(context.Context, *Credentials) (bool, error) {
		return false, errors.New("temporary network failure")
	}
	e.mu.Unlock()
	if _, err := m.Do(context.Background(), key, htmlfetch.Request{URL: srv.URL}); err == nil {
		t.Fatal("check error swallowed")
	}
	if logins.Load() != 1 {
		t.Fatal("network error triggered login")
	}
}

func TestAccountLogoutDuringLoginPreventsLatePublication(t *testing.T) {
	m := NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	key := Key{"site", "alice"}
	started, release := make(chan struct{}), make(chan struct{})
	register(t, m, key, Config{Origins: []string{"https://example.com"}, Login: func(context.Context, *Credentials) error { close(started); <-release; return nil }, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }})
	e, _ := m.get(key)
	done := make(chan error, 1)
	go func() { _, _, err := e.ensure(context.Background()); done <- err }()
	<-started
	if err := m.Logout(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(key); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	if m.Status()[0].HTTPReady {
		t.Fatal("late login resurrected logged-out credentials")
	}
}
