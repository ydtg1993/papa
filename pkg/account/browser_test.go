package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func accountBrowserPool(t *testing.T, origins ...string) *browser.Pool {
	t.Helper()
	pool, err := browser.NewPool(browser.PoolConfig{Network: egress.Config{PrivateOrigins: origins}, Size: 1, Headless: true, NoSandbox: true, Leakless: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type accountBrowserSite struct {
	server   *httptest.Server
	client   *htmlfetch.Client
	logins   atomic.Int32
	checks   atomic.Int32
	requests atomic.Int32
}

func newAccountBrowserSite(t *testing.T) *accountBrowserSite {
	t.Helper()
	s := &accountBrowserSite{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.URL.Path == "/api/check" {
				s.checks.Add(1)
			} else {
				s.requests.Add(1)
			}
			sid, sidErr := r.Cookie("sid")
			api, apiErr := r.Cookie("api-session")
			if sidErr != nil || apiErr != nil || api.Value != sid.Value || r.Header.Get("Authorization") != "Bearer token-"+sid.Value {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, sid.Value)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		sid := ""
		if c, err := r.Cookie("sid"); err == nil {
			sid = c.Value
		}
		quotedSID, _ := json.Marshal(sid)
		if r.URL.Path == "/login" {
			account := r.URL.Query().Get("account")
			quotedAccount, _ := json.Marshal(account)
			s.logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: account, Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "api-session", Value: account, Path: "/api", HttpOnly: true})
			_, _ = fmt.Fprintf(w, `<script>localStorage.setItem('account',%s);localStorage.setItem('token','token-'+%s)</script>`, quotedAccount, quotedAccount)
		}
		_, _ = fmt.Fprintf(w, `<script>window.firstAccount=localStorage.getItem('account')||'';window.firstSID=%s;</script>`, quotedSID)
	}))
	s.client = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{s.server.URL}}})
	t.Cleanup(s.server.Close)
	return s
}

func (s *accountBrowserSite) config(pool *browser.Pool, name string) Config {
	return Config{
		Origins: []string{s.server.URL}, Timeout: 15 * time.Second, CheckInterval: time.Hour,
		Browser: &BrowserConfig{
			Pool: pool, URL: s.server.URL + "/bootstrap",
			CookieURLs: []string{s.server.URL + "/api/check"},
			Login: func(_ context.Context, page *rod.Page) error {
				initial, err := page.Eval(`() => firstAccount+'|'+firstSID`)
				if err != nil {
					return err
				}
				if initial.Value.Str() != "|" {
					return fmt.Errorf("fresh account login inherited state: %q", initial.Value.Str())
				}
				if err := page.Navigate(s.server.URL + "/login?account=" + url.QueryEscape(name)); err != nil {
					return err
				}
				return page.WaitLoad()
			},
			Check: func(_ context.Context, page *rod.Page) (bool, error) {
				value, err := page.Eval(`() => localStorage.getItem('account')`)
				if err != nil || value.Value.Str() != name {
					return false, err
				}
				cookies, err := page.Cookies(nil)
				if err != nil {
					return false, err
				}
				for _, cookie := range cookies {
					if cookie.Name == "sid" && cookie.Value == name && cookie.HTTPOnly {
						return true, nil
					}
				}
				return false, nil
			},
		},
	}
}

func (s *accountBrowserSite) bridgeConfig(pool *browser.Pool, name string) Config {
	cfg := s.config(pool, name)
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		r, err := s.client.Do(c.Context(ctx), htmlfetch.Request{URL: s.server.URL + "/api/check", NoRedirect: true})
		if r != nil && r.StatusCode == http.StatusUnauthorized {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return string(r.Body) == name, nil
	}
	cfg.Browser.Export = func(_ context.Context, page *rod.Page, c *Credentials) error {
		token, err := page.Eval(`() => localStorage.getItem('token')`)
		if err == nil {
			c.Headers["Authorization"] = "Bearer " + token.Value.Str()
		}
		return err
	}
	return cfg
}

func accountBrowserStatus(t *testing.T, m *Manager, key Key) Status {
	t.Helper()
	for _, status := range m.Status() {
		if status.Key == key {
			return status
		}
	}
	t.Fatal("account status missing")
	return Status{}
}

func TestAccountBrowserStateReuseIsolationAndPause(t *testing.T) {
	s := newAccountBrowserSite(t)
	pool := accountBrowserPool(t, s.server.URL)
	m := NewManager(nil) // A browser-only account requires no HTTP client/provider.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"alice", "bob"} {
		key := Key{Site: "browser", ID: name}
		register(t, m, key, s.config(pool, name))
		before := accountBrowserStatus(t, m, key).Version
		for visit := 0; visit < 2; visit++ {
			if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
				if visit == 1 {
					initial, err := page.Eval(`() => firstAccount+'|'+firstSID+'|'+localStorage.getItem('business')`)
					if err != nil {
						return err
					}
					if want := name + "|" + name + "|saved-" + name; initial.Value.Str() != want {
						return fmt.Errorf("restored account %q, want %q", initial.Value.Str(), want)
					}
				}
				_, err := page.Eval(`(value) => localStorage.setItem('business',value)`, "saved-"+name)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		status := accountBrowserStatus(t, m, key)
		if status.Version != before || status.HTTPReady || !status.BrowserReady || status.LastBrowserCheck.IsZero() {
			t.Fatalf("browser activity changed HTTP generation/readiness: %+v", status)
		}
		if err := m.Pause(key); err != nil {
			t.Fatal(err)
		}
		called := false
		if err := m.WithBrowser(ctx, key, func(*rod.Page) error { called = true; return nil }); !errors.Is(err, ErrPaused) || called {
			t.Fatalf("paused browser account executed: called=%v err=%v", called, err)
		}
	}
	if s.logins.Load() != 2 {
		t.Fatalf("restored accounts logged in again: %d", s.logins.Load())
	}
}

func TestAccountBrowserCallbackErrorIsNotReplayedOrSaved(t *testing.T) {
	s := newAccountBrowserSite(t)
	m := NewManager(nil)
	key := Key{Site: "browser", ID: "alice"}
	register(t, m, key, s.config(accountBrowserPool(t, s.server.URL), key.ID))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		_, err := page.Eval(`() => localStorage.setItem('business','saved')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("business parse failed")
	called := 0
	err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		called++
		if _, err := page.Eval(`() => localStorage.setItem('business','discard-this')`); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) || called != 1 {
		t.Fatalf("business callback replayed or error lost: called=%d err=%v", called, err)
	}
	if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		value, err := page.Eval(`() => localStorage.getItem('business')`)
		if err == nil && value.Value.Str() != "saved" {
			return errors.New("failed callback state was published")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if s.logins.Load() != 1 {
		t.Fatal("business failure triggered authentication retry")
	}
	expiredCalls := 0
	err = m.WithBrowser(ctx, key, func(*rod.Page) error {
		expiredCalls++
		return ErrExpired
	})
	if !errors.Is(err, ErrExpired) || expiredCalls != 1 || s.logins.Load() != 1 {
		t.Fatalf("business expiry was treated as restored-state retry: calls=%d logins=%d err=%v", expiredCalls, s.logins.Load(), err)
	}
}

func TestAccountBrowserStoreRestart(t *testing.T) {
	s := newAccountBrowserSite(t)
	pool := accountBrowserPool(t, s.server.URL)
	store, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.config(pool, "alice")
	cfg.Store = store
	key := Key{Site: "browser", ID: "alice"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for restart := 0; restart < 2; restart++ {
		m := NewManager(nil)
		register(t, m, key, cfg)
		if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
			if restart == 1 {
				value, err := page.Eval(`() => localStorage.getItem('restart')`)
				if err != nil || value.Value.Str() != "persisted" {
					return fmt.Errorf("browser restart state missing: %v", err)
				}
			}
			_, err := page.Eval(`() => localStorage.setItem('restart','persisted')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if s.logins.Load() != 1 {
		t.Fatal("stored browser state did not survive manager restart")
	}
}

func TestAccountBrowserHTTPBridgeAndGeneration(t *testing.T) {
	s := newAccountBrowserSite(t)
	m := NewManager(s.client)
	key := Key{Site: "bridge", ID: "alice"}
	cfg := s.bridgeConfig(accountBrowserPool(t, s.server.URL), key.ID)
	check := cfg.Check
	stateAtCheck := false
	cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
		stateAtCheck = c.BrowserState != nil
		return check(ctx, c)
	}
	register(t, m, key, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := htmlfetch.Request{URL: s.server.URL + "/api/data"}
	response, err := m.Do(ctx, key, request)
	if err != nil || response == nil || string(response.Body) != key.ID {
		t.Fatalf("Rod cookie/token bridge failed: response=%v err=%v", response, err)
	}
	before := accountBrowserStatus(t, m, key)
	if !before.HTTPReady || !before.BrowserReady || before.Version == 0 || s.checks.Load() != 1 || !stateAtCheck {
		t.Fatalf("bridge published without HTTP verification/state: %+v checks=%d stateAtCheck=%v", before, s.checks.Load(), stateAtCheck)
	}
	if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		initial, err := page.Eval(`() => firstAccount+'|'+firstSID`)
		if err != nil {
			return err
		}
		if initial.Value.Str() != "alice|alice" {
			return errors.New("bridge did not save browser state")
		}
		_, err = page.Eval(`() => localStorage.setItem('token','browser-only-change')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response, err = m.Do(ctx, key, request)
	if err != nil || response == nil || string(response.Body) != key.ID {
		t.Fatalf("browser activity overwrote HTTP credentials: %v", err)
	}
	after := accountBrowserStatus(t, m, key)
	if after.Version != before.Version || s.logins.Load() != 1 || s.checks.Load() != 1 || s.requests.Load() != 2 {
		t.Fatalf("browser activity changed HTTP generation: before=%+v after=%+v logins=%d checks=%d requests=%d", before, after, s.logins.Load(), s.checks.Load(), s.requests.Load())
	}
}

func TestAccountBrowserHTTPBridgeRejectsUnverifiedToken(t *testing.T) {
	s := newAccountBrowserSite(t)
	m := NewManager(s.client)
	key := Key{Site: "bridge", ID: "alice"}
	cfg := s.bridgeConfig(accountBrowserPool(t, s.server.URL), key.ID)
	cfg.Browser.Export = func(_ context.Context, _ *rod.Page, c *Credentials) error {
		c.Headers["Authorization"] = "Bearer wrong-token"
		return nil
	}
	register(t, m, key, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/data"}); !errors.Is(err, ErrExpired) {
			t.Fatalf("unverified bridge token accepted: %v", err)
		}
	}
	status := accountBrowserStatus(t, m, key)
	if status.HTTPReady || status.BrowserReady || status.Version != 0 || s.requests.Load() != 0 || s.logins.Load() != 1 || s.checks.Load() != 1 {
		t.Fatalf("failed bridge published/retried credentials: %+v login=%d check=%d requests=%d", status, s.logins.Load(), s.checks.Load(), s.requests.Load())
	}
}

func TestAccountBrowserOriginBoundaries(t *testing.T) {
	s := newAccountBrowserSite(t)
	pool := accountBrowserPool(t, s.server.URL)
	var foreignRequests atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		_, _ = io.WriteString(w, "<html>foreign origin</html>")
	}))
	defer other.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []string{"bootstrap", "cookies"} {
		cfg := s.bridgeConfig(pool, "alice")
		if kind == "bootstrap" {
			cfg.Browser.URL = other.URL
		} else {
			cfg.Browser.CookieURLs = []string{other.URL}
		}
		if err := NewManager(s.client).Register(Key{Site: "scope", ID: kind}, cfg); err == nil {
			t.Fatalf("foreign %s scope accepted", kind)
		}
	}
	m := NewManager(s.client)
	key := Key{Site: "scope", ID: "http"}
	register(t, m, key, s.bridgeConfig(pool, "alice"))
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: other.URL}); err == nil || foreignRequests.Load() != 0 || s.logins.Load() != 0 {
		t.Fatalf("HTTP origin boundary crossed: err=%v foreign=%d logins=%d", err, foreignRequests.Load(), s.logins.Load())
	}

	for _, mode := range []string{"login", "bridge-login", "check", "export", "business"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager(s.client)
			cfg := s.bridgeConfig(pool, "alice")
			key := Key{Site: "scope", ID: mode}
			var checks, exports, callbacks int
			originalCheck := cfg.Browser.Check
			cfg.Browser.Check = func(ctx context.Context, page *rod.Page) (bool, error) {
				checks++
				if mode == "check" {
					if err := page.Navigate(other.URL); err != nil {
						return false, err
					}
					return true, page.WaitLoad()
				}
				return originalCheck(ctx, page)
			}
			if mode == "login" || mode == "bridge-login" {
				cfg.Browser.Login = func(_ context.Context, page *rod.Page) error {
					if err := page.Navigate(other.URL); err != nil {
						return err
					}
					return page.WaitLoad()
				}
			}
			originalExport := cfg.Browser.Export
			cfg.Browser.Export = func(ctx context.Context, page *rod.Page, c *Credentials) error {
				exports++
				if err := originalExport(ctx, page, c); err != nil {
					return err
				}
				if mode == "export" {
					if err := page.Navigate(other.URL); err != nil {
						return err
					}
					return page.WaitLoad()
				}
				return nil
			}
			register(t, m, key, cfg)
			var err error
			if mode == "export" || mode == "bridge-login" {
				_, err = m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/data"})
			} else {
				err = m.WithBrowser(ctx, key, func(page *rod.Page) error {
					callbacks++
					if mode == "business" {
						if err := page.Navigate(other.URL); err != nil {
							return err
						}
						return page.WaitLoad()
					}
					return nil
				})
			}
			status := accountBrowserStatus(t, m, key)
			if err == nil || status.HTTPReady || status.BrowserReady || (mode != "business" && callbacks != 0) {
				t.Fatalf("browser %s origin boundary crossed: err=%v callbacks=%d status=%+v", mode, err, callbacks, status)
			}
			if mode == "login" && checks != 1 {
				t.Fatalf("Check ran after login left scope: %d", checks)
			}
			if mode == "bridge-login" && checks != 0 {
				t.Fatalf("bridge Check ran after login left scope: %d", checks)
			}
			if mode != "export" && exports != 0 {
				t.Fatal("unexpected browser Export")
			}
		})
	}
	if s.checks.Load() != 0 || s.requests.Load() != 0 {
		t.Fatal("cross-origin browser state reached HTTP verification or business request")
	}
}

func TestAccountBrowserBootstrapRedirectNeverRunsLogin(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "foreign origin")
	}))
	defer other.Close()
	bootstrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer bootstrap.Close()
	pool := accountBrowserPool(t, bootstrap.URL, other.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, bridge := range []bool{false, true} {
		t.Run(fmt.Sprint(bridge), func(t *testing.T) {
			m := NewManager(htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{other.URL, bootstrap.URL}}}))
			key := Key{Site: "redirect", ID: fmt.Sprint(bridge)}
			callbacks := 0
			cfg := Config{Origins: []string{bootstrap.URL}, Browser: &BrowserConfig{
				Pool: pool, URL: bootstrap.URL,
				Login:  func(context.Context, *rod.Page) error { callbacks++; return nil },
				Check:  func(context.Context, *rod.Page) (bool, error) { callbacks++; return true, nil },
				Export: func(context.Context, *rod.Page, *Credentials) error { callbacks++; return nil },
			}}
			if bridge {
				cfg.Check = func(context.Context, *Credentials) (bool, error) { callbacks++; return true, nil }
			}
			register(t, m, key, cfg)
			var err error
			if bridge {
				_, err = m.Do(ctx, key, htmlfetch.Request{URL: bootstrap.URL})
			} else {
				err = m.WithBrowser(ctx, key, func(*rod.Page) error { callbacks++; return nil })
			}
			if err == nil || callbacks != 0 {
				t.Fatalf("bootstrap redirect invoked account provider: callbacks=%d err=%v", callbacks, err)
			}
		})
	}
}
