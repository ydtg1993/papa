package account

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func TestAccountBridgeAndBrowserSerializeAndReusePublishedState(t *testing.T) {
	for _, first := range []string{"bridge-login", "bridge-verification", "browser"} {
		t.Run(first, func(t *testing.T) {
			s := newAccountBrowserSite(t)
			pool, err := browser.NewPool(browser.PoolConfig{Network: egress.Config{PrivateOrigins: []string{s.server.URL}}, Size: 2, Headless: true, NoSandbox: true, Leakless: false})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			block := func() error {
				if !blocked.CompareAndSwap(false, true) {
					return nil
				}
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			cfg := s.bridgeConfig(pool, "alice")
			cfg.MaxConcurrent = 2
			var active, peak, callbacks atomic.Int32
			login := cfg.Browser.Login
			cfg.Browser.Login = func(ctx context.Context, page *rod.Page) error {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); old < n && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				if first == "bridge-login" {
					if err := block(); err != nil {
						return err
					}
				}
				return login(ctx, page)
			}
			check := cfg.Check
			cfg.Check = func(ctx context.Context, c *Credentials) (bool, error) {
				if first == "bridge-verification" {
					if err := block(); err != nil {
						return false, err
					}
				}
				return check(ctx, c)
			}
			m := NewManager(s.client)
			key := Key{Site: "concurrent", ID: first}
			register(t, m, key, cfg)
			bridge := func() error {
				_, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/data"})
				return err
			}
			browse := func() error {
				return m.WithBrowser(ctx, key, func(page *rod.Page) error {
					callbacks.Add(1)
					if first == "browser" {
						if err := block(); err != nil {
							return err
						}
					}
					_, err := page.Eval(`() => localStorage.setItem('business','published')`)
					return err
				})
			}
			start, next := bridge, browse
			if first == "browser" {
				start, next = browse, bridge
			}
			done := make(chan error, 2)
			go func() { done <- start() }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first operation did not reach barrier", ctx.Err())
			}
			go func() { done <- next() }()
			// The second pool member makes an accidental concurrent login observable.
			timer := time.NewTimer(600 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
			close(release)
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Errorf("operation failed: %v", err)
				}
			}
			if peak.Load() != 1 || s.logins.Load() != 1 || callbacks.Load() != 1 {
				t.Errorf("overlap or duplicate work: peak=%d logins=%d callbacks=%d", peak.Load(), s.logins.Load(), callbacks.Load())
			}
			if status := accountBrowserStatus(t, m, key); !status.HTTPReady || !status.BrowserReady || status.Version != 1 {
				t.Errorf("publication lost or HTTP generation changed: %+v", status)
			}
			if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
				value, err := page.Eval(`() => localStorage.getItem('business')`)
				if err == nil && value.Value.Str() != "published" {
					return fmt.Errorf("latest browser snapshot lost: %q", value.Value.Str())
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAccountBrowserSlotCancellationAndHTTPFastPath(t *testing.T) {
	s := newAccountBrowserSite(t)
	m := NewManager(s.client)
	key := Key{Site: "concurrent", ID: "cancel"}
	cfg := s.bridgeConfig(accountBrowserPool(t, s.server.URL), "alice")
	cfg.MaxConcurrent = 2
	cfg.Update = func(c *Credentials, _ *htmlfetch.Response) error {
		c.Values["updated"] += "x"
		return nil
	}
	register(t, m, key, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.WithBrowser(ctx, key, func(*rod.Page) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err := m.Do(short, key, htmlfetch.Request{URL: s.server.URL + "/api/data"})
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || s.logins.Load() != 1 {
		t.Errorf("canceled bridge waiter performed login: logins=%d err=%v", s.logins.Load(), err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/data"}); err != nil {
		t.Fatal(err)
	}
	before := accountBrowserStatus(t, m, key).Version
	// The HTTP cache-hit path, including a credential update, must complete
	// while WithBrowser owns its slot and must not supersede its snapshot.
	entered, release = make(chan struct{}), make(chan struct{})
	go func() {
		done <- m.WithBrowser(ctx, key, func(*rod.Page) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	call, stop := context.WithTimeout(ctx, 2*time.Second)
	_, err = m.Do(call, key, htmlfetch.Request{URL: s.server.URL + "/api/data"})
	stop()
	close(release)
	if err != nil {
		t.Error(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after := accountBrowserStatus(t, m, key); after.Version != before+1 || !after.BrowserReady || s.logins.Load() != 1 {
		t.Fatalf("independent HTTP/browser publication failed: before=%d after=%+v logins=%d", before, after, s.logins.Load())
	}
}

func TestAccountBrowserBridgeRetriesHTTPExpiredSnapshotOnce(t *testing.T) {
	s := newAccountBrowserSite(t)
	m := NewManager(s.client)
	key := Key{Site: "bridge", ID: "expired-http"}
	register(t, m, key, s.bridgeConfig(accountBrowserPool(t, s.server.URL), "alice"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.WithBrowser(ctx, key, func(page *rod.Page) error {
		_, err := page.Eval(`() => localStorage.setItem('token','expired-http-token')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/data"}); err != nil {
		t.Fatal(err)
	}
	if s.logins.Load() != 2 || s.checks.Load() != 2 {
		t.Fatalf("HTTP expiry did not get one clean login: logins=%d checks=%d", s.logins.Load(), s.checks.Load())
	}
}
