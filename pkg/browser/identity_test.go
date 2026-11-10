package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func identityTestProfile(id string) identity.Profile {
	return identity.Profile{Site: "fixture", ID: id, Version: 1, SchemaVersion: identity.SchemaVersion, UAPolicy: identity.NativeChromium, Languages: []string{"en-US", "en"}, Locale: "en-US", Timezone: "Asia/Shanghai", Viewport: identity.Size{Width: 1280, Height: 720}, Screen: identity.Size{Width: 1920, Height: 1080}, DeviceScaleFactor: 1.25, Touch: true, MaxTouchPoints: 3, Adapter: identity.AdapterConfig{ID: "webdriver", Version: "1", Enabled: true}}
}

func TestIdentityDiagnosticConfigExcludesCredentialsAndEgress(t *testing.T) {
	device := devices.LaptopWithMDPIScreen
	device.Capabilities = []string{"touch"}
	p, err := NewPool(PoolConfig{Size: 1, Headless: true, Flags: map[string]string{"proxy-server": "http://user:secret@proxy.invalid", "user-agent": "secret-agent", "disable-gpu": ""}, DefaultDevice: &device, DefaultHeaders: map[string]string{"Authorization": "secret", "User-Agent": "fixture-native-declaration"}, Network: egress.Config{PrivateOrigins: []string{"http://127.0.0.1:1234"}, AllowedOrigins: []string{"http://127.0.0.1:1234"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, omissions := p.DiagnosticConfig()
	data, _ := json.Marshal(c)
	if strings.Contains(string(data), "secret") || len(c.Network.PrivateOrigins) != 0 || len(c.Network.AllowedOrigins) != 0 || len(c.DefaultHeaders) != 1 || c.DefaultHeaders["User-Agent"] != "fixture-native-declaration" || c.DefaultDevice == nil || c.ProxyManager != nil || len(omissions) < 2 {
		t.Fatalf("unsafe diagnostic clone: %s", data)
	}
	c.Flags["disable-gpu"] = "changed"
	c.DefaultDevice.Capabilities[0] = "changed"
	if device.Capabilities[0] != "touch" {
		t.Fatal("diagnostic device alias")
	}
	if p.cfg.Flags["disable-gpu"] != "" {
		t.Fatal("flags alias")
	}
}

func TestIdentityFirstScriptHeadersStateAndProtection(t *testing.T) {
	bin := os.Getenv("PAPA_TEST_CHROME")
	if bin == "" {
		bin = os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	}
	if bin == "" {
		t.Skip("PAPA_TEST_CHROME not configured")
	}
	var mu sync.Mutex
	requests := []http.Header{}
	var expectedUA, expectedPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/first", 302)
			return
		}
		if r.URL.Path != "/first" {
			w.WriteHeader(404)
			return
		}
		mu.Lock()
		requests = append(requests, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Accept-CH", "Sec-CH-UA-Full-Version-List, Sec-CH-UA-Platform-Version, Sec-CH-UA-Arch")
		fmt.Fprint(w, `<!doctype html><script>window.first=(`+identity.FirstScript+`)();window.first.token=localStorage.getItem('token');</script><div id="ready">ready</div>`)
	}))
	defer srv.Close()
	p, err := NewPool(PoolConfig{Size: 1, Headless: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	err = p.WithIsolatedBrowser(ctx, func(b *Browser) error {
		r, err := b.ResolveIdentity(ctx, identityTestProfile("a"))
		if err != nil {
			return err
		}
		frozen := r.Clone()
		expectedUA, expectedPlatform = frozen.UserAgent, frozen.UserAgentMetadata.Platform
		return b.WithStatePage(ctx, srv.URL+"/start", navigationState(srv.URL, "saved"), PageOptions{Identity: r, Headers: map[string]string{"Authorization": "Bearer fixture", "X-First": "one"}}, func(page *rod.Page) error {
			r.Profile.Languages[0] = "fr-FR" // caller no longer owns the installed snapshot
			first, err := page.Eval(`() => window.first`)
			if err != nil {
				return err
			}
			var o identity.Observation
			if err = first.Value.Unmarshal(&o); err != nil {
				return err
			}
			if o.UserAgent != frozen.UserAgent || o.Platform != frozen.Platform || o.Language != "en-US" || strings.Join(o.Languages, ",") != "en-US,en" || o.Locale != "en-US" || o.Timezone != "Asia/Shanghai" || o.Viewport != frozen.Profile.Viewport || o.Screen != frozen.Profile.Screen || o.DeviceScaleFactor != 1.25 || o.MaxTouchPoints != 3 || o.Webdriver || !o.ClientHintsAvailable {
				return fmt.Errorf("first script mismatch: %+v", o)
			}
			if first.Value.Get("token").Str() != "saved" {
				return fmt.Errorf("identity adapter broke state restoration")
			}
			observed, err := ObserveIdentity(page)
			if err != nil {
				return err
			}
			if !observed.Adapter.Registered || !observed.Adapter.Executed || !observed.Adapter.Verified || observed.HardwareConcurrency < 1 {
				return fmt.Errorf("observation lacks native/adapter execution evidence: %+v", observed)
			}
			for name, opts := range map[string]PageOptions{"UA": {Headers: map[string]string{"user-agent": "wrong"}}, "CH": {Headers: map[string]string{"Sec-CH-UA-Platform": "fake"}}, "device": {Device: &devices.LaptopWithMDPIScreen}, "reference": {Identity: r}} {
				if err := ConfigurePage(ctx, page, opts); !errors.Is(err, identity.ErrConflict) {
					return fmt.Errorf("%s conflict accepted: %v", name, err)
				}
			}
			if err := NavigatePage(ctx, page, srv.URL+"/first", PageOptions{Identity: frozen, Headers: map[string]string{"X-Second": "two"}}); err != nil {
				return err
			}
			observed, err = ObserveIdentity(page)
			if err != nil {
				return err
			}
			if !observed.Adapter.Verified || observed.Language != "en-US" {
				return fmt.Errorf("identity did not survive navigation")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("missing requests: %d", len(requests))
	}
	for _, h := range requests {
		if h.Get("Authorization") != "Bearer fixture" || h.Get("User-Agent") != expectedUA || !strings.HasPrefix(h.Get("Accept-Language"), "en-US,en") || h.Get("Sec-CH-UA") == "" || h.Get("Sec-CH-UA-Platform") != `"`+expectedPlatform+`"` {
			t.Fatalf("first request identity/auth mismatch: %v", h)
		}
	}
	if requests[len(requests)-1].Get("X-Second") != "two" {
		t.Fatal("ordinary headers not applied")
	}
}

func TestIdentityRuntimeMismatchAndInitialConflict(t *testing.T) {
	bin := os.Getenv("PAPA_TEST_CHROME")
	if bin == "" {
		t.Skip("PAPA_TEST_CHROME not configured")
	}
	p, err := NewPool(PoolConfig{Size: 1, Headless: true, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = p.WithBrowser(ctx, func(b *Browser) error {
		r, err := b.ResolveIdentity(ctx, identityTestProfile("a"))
		if err != nil {
			return err
		}
		for _, opts := range []PageOptions{{Identity: r, Headers: map[string]string{"Accept-Language": "zh-CN"}}, {Identity: r, RemoveHeaders: []string{"User-Agent"}}, {Identity: r, Device: &devices.LaptopWithMDPIScreen}} {
			if err := b.WithPageOptions(ctx, opts, func(*rod.Page) error { return nil }); !errors.Is(err, identity.ErrConflict) {
				return fmt.Errorf("initial conflict accepted: %v", err)
			}
		}
		bad := r.Clone()
		bad.Runtime.Product = "Chrome/1.0.0.0"
		if err = identity.CheckRuntime(ctx, b.Browser, bad); !errors.Is(err, identity.ErrIncompatible) {
			return fmt.Errorf("runtime mismatch accepted: %v", err)
		}
		var key identityPageKey
		if err = b.WithPageOptions(ctx, PageOptions{Identity: r}, func(page *rod.Page) error {
			key = pageIdentityKey(page)
			if ManagedIdentity(page) == nil {
				return fmt.Errorf("identity not tracked")
			}
			return nil
		}); err != nil {
			return err
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := identityPages.Load(key); !ok {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("closed page identity was retained")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIdentityTwoAccountsSameBrowserRemainIndependent(t *testing.T) {
	bin := os.Getenv("PAPA_TEST_CHROME")
	if bin == "" {
		t.Skip("PAPA_TEST_CHROME not configured")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<script>window.first=(`+identity.FirstScript+`)()</script>`)
	}))
	defer srv.Close()
	p, err := NewPool(PoolConfig{Size: 1, Headless: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = p.WithBrowser(ctx, func(parent *Browser) error {
		return parent.withIsolatedContext(ctx, func(a *Browser) error {
			pa := identityTestProfile("account-a")
			ra, err := a.ResolveIdentity(ctx, pa)
			if err != nil {
				return err
			}
			return a.WithPageOptions(ctx, PageOptions{Identity: ra}, func(pageA *rod.Page) error {
				return parent.withIsolatedContext(ctx, func(b *Browser) error {
					pb := identityTestProfile("account-b")
					pb.Languages = []string{"zh-CN", "zh"}
					pb.Locale = "zh-CN"
					pb.Timezone = "UTC"
					pb.Viewport = identity.Size{Width: 900, Height: 600}
					pb.DeviceScaleFactor = 2
					pb.Touch = false
					pb.MaxTouchPoints = 0
					pb.Adapter = identity.AdapterConfig{}
					rb, err := b.ResolveIdentity(ctx, pb)
					if err != nil {
						return err
					}
					return b.WithPageOptions(ctx, PageOptions{Identity: rb}, func(pageB *rod.Page) error {
						for _, item := range []struct {
							page *rod.Page
							p    identity.Profile
						}{{pageA, pa}, {pageB, pb}, {pageA, pa}} {
							if err := NavigatePage(ctx, item.page, srv.URL, PageOptions{}); err != nil {
								return err
							}
							v, err := item.page.Eval(`() => window.first`)
							if err != nil {
								return err
							}
							var observed identity.Observation
							if err = v.Value.Unmarshal(&observed); err != nil {
								return err
							}
							if observed.Language != item.p.Languages[0] || observed.Locale != item.p.Locale || observed.Timezone != item.p.Timezone || observed.Viewport != item.p.Viewport || observed.DeviceScaleFactor != item.p.DeviceScaleFactor || observed.MaxTouchPoints != item.p.MaxTouchPoints {
								return fmt.Errorf("cross-account identity contamination: %+v", observed)
							}
						}
						return nil
					})
				})
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
