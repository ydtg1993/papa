package account

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func accountIdentityProfile(site string, version uint64) identity.Profile {
	language := "en-US"
	if version == 2 {
		language = "fr-FR"
	}
	return identity.Profile{Site: site, ID: "desktop", Version: version, SchemaVersion: 1, UAPolicy: identity.NativeChromium, Languages: []string{language}, Locale: language, Timezone: "UTC", Viewport: identity.Size{Width: 800, Height: 600}, Screen: identity.Size{Width: 1280, Height: 720}, DeviceScaleFactor: 1}
}

func TestIdentityAccountBrowserBridgeRestartAndFailedSwitch(t *testing.T) {
	path := os.Getenv("PAPA_TEST_CHROME")
	if path == "" {
		t.Skip("set PAPA_TEST_CHROME for isolated local Chromium acceptance")
	}
	s := newAccountBrowserSite(t)
	pool, err := browser.NewPool(browser.PoolConfig{BrowserPath: path, Size: 1, Headless: true, NoSandbox: true, Network: egress.Config{PrivateOrigins: []string{s.server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	credentials, err := NewFileStore(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := identityruntime.NewMemoryStore()
	key := Key{Site: "fixture", ID: "alice"}
	subject := identityruntime.Subject{Site: key.Site, Kind: "account", Key: key.ID}
	for n := uint64(1); n <= 2; n++ {
		if err = store.AppendProfile(context.Background(), accountIdentityProfile(key.Site, n), n-1); err != nil {
			t.Fatal(err)
		}
	}
	var reject atomic.Bool
	var observed atomic.Int32
	makeManager := func() (*Manager, *identityruntime.Manager) {
		m := NewManager(s.client)
		cfg := s.bridgeConfig(pool, key.ID)
		cfg.Store = credentials
		originalLogin := cfg.Browser.Login
		cfg.Browser.Login = func(ctx context.Context, page *rod.Page) error {
			value, err := page.Eval(`() => navigator.language`)
			if err != nil {
				return err
			}
			if value.Value.Str() != "en-US" && value.Value.Str() != "fr-FR" {
				t.Errorf("login saw wrong identity %q", value.Value.Str())
			}
			observed.Add(1)
			if reject.Load() {
				return errors.New("synthetic identity verification failure")
			}
			return originalLogin(ctx, page)
		}
		if err := m.Register(key, cfg); err != nil {
			t.Fatal(err)
		}
		rt, err := identityruntime.NewManager(store, identityruntime.Options{Resolve: m.ResolveIdentity, Verify: m.VerifyIdentity, VerifyTimeout: 40 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		m.SetIdentityManager(rt)
		t.Cleanup(rt.Close)
		return m, rt
	}
	m, rt := makeManager()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, err := rt.Stage(ctx, subject, accountIdentityProfile(key.Site, 1).Reference(), 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err = rt.Reverify(ctx, subject, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Load() != 1 {
		t.Fatalf("identity activation did not authenticate under frozen profile: %d", observed.Load())
	}
	if _, err = m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL + "/api/check"}); err != nil {
		t.Fatal(err)
	}
	if err = m.WithBrowser(ctx, key, func(p *rod.Page) error {
		v, err := p.Eval(`() => navigator.language+'|'+firstAccount`)
		if err == nil && v.Value.Str() != "en-US|alice" {
			t.Errorf("wrong frozen identity/state %q", v.Value.Str())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := credentials.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus := m.Status()[0]
	b, err = rt.Stage(ctx, subject, accountIdentityProfile(key.Site, 2).Reference(), b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(true)
	b, err = rt.Reverify(ctx, subject, b.Revision)
	if !errors.Is(err, identityruntime.ErrVerification) {
		t.Fatal(err)
	}
	after, loadErr := credentials.Load(ctx, key)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !reflect.DeepEqual(before, after) || m.Status()[0].Version != beforeStatus.Version {
		t.Fatal("failed switch changed credentials or account version")
	}
	if b.Active.Version != 1 || b.Pending.Version != 2 {
		t.Fatal(b)
	}
	b, err = rt.Cancel(ctx, subject, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(false)
	// A fresh registry loads the durable metadata and verifies it before use.
	restarted, _ := makeManager()
	if err = restarted.WithBrowser(ctx, key, func(p *rod.Page) error {
		v, err := p.Eval(`() => navigator.language+'|'+firstAccount`)
		if err == nil && v.Value.Str() != "en-US|alice" {
			t.Errorf("restart mixed identity/state %q", v.Value.Str())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if observed.Load() != 2 {
		t.Fatalf("restart unnecessarily relogged identity-matching state: %d", observed.Load())
	}
}

func TestIdentityAccountSnapshotAndDiagnosisConfigDoNotAuthenticate(t *testing.T) {
	m := NewManager(nil)
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var callbacks atomic.Int32
	key := Key{Site: "fixture", ID: "alice"}
	if err = m.Register(key, Config{Origins: []string{"https://example.invalid"}, Browser: &BrowserConfig{Pool: pool, URL: "https://example.invalid", Login: func(context.Context, *rod.Page) error { callbacks.Add(1); return nil }, Check: func(context.Context, *rod.Page) (bool, error) { callbacks.Add(1); return true, nil }}}); err != nil {
		t.Fatal(err)
	}
	m.Status()
	if _, _, err = m.DiagnosticBrowserConfig(key); err != nil {
		t.Fatal(err)
	}
	if callbacks.Load() != 0 {
		t.Fatal("read-only account inspection invoked authentication")
	}
}

func TestIdentityInitializationFailurePreservesAccountCredentials(t *testing.T) {
	manager := NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	key := Key{Site: "fixture", ID: "alice"}
	initialization := &identity.InitializationError{Stage: "locale", Cause: errors.New("synthetic CDP failure")}
	if err := manager.Register(key, Config{Origins: []string{"https://example.invalid"}, Login: func(context.Context, *Credentials) error { return nil }, Check: func(context.Context, *Credentials) (bool, error) { return false, initialization }}); err != nil {
		t.Fatal(err)
	}
	entry, err := manager.get(key)
	if err != nil {
		t.Fatal(err)
	}
	original := &Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{"Authorization": "original-token"}, Values: map[string]string{"refresh": "original-refresh"}}
	entry.current = original
	entry.version = 7
	if _, _, err = entry.ensure(context.Background()); !errors.Is(err, initialization) {
		t.Fatal(err)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.current != original || entry.version != 7 || entry.failure != nil || entry.paused || !entry.retryAt.IsZero() {
		t.Fatal("identity initialization failure changed account credentials or cooldown")
	}
	if !identityOrSiteFailure(&Failure{Code: "browser-check-failed", Cause: initialization}) {
		t.Fatal("generic CDP initialization failure lost its independent classification")
	}
}

type identityPublishFault struct {
	identityruntime.Store
	fail   bool
	commit bool
}

func (s *identityPublishFault) SaveBindingCAS(ctx context.Context, b identityruntime.Binding, expected uint64) error {
	if s.fail && b.Pending == nil && b.Active != nil && b.Active.Version == 2 {
		if s.commit {
			if err := s.Store.SaveBindingCAS(ctx, b, expected); err != nil {
				return err
			}
		}
		return identityruntime.ErrUncertain
	}
	return s.Store.SaveBindingCAS(ctx, b, expected)
}

func TestIdentityAccountDurableCandidateSurvivesUncertainPublish(t *testing.T) {
	path := os.Getenv("PAPA_TEST_CHROME")
	if path == "" {
		t.Skip("set PAPA_TEST_CHROME for isolated local Chromium acceptance")
	}
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-commit-cancel", true: "after-commit-restart"}[commit], func(t *testing.T) {
			s := newAccountBrowserSite(t)
			pool, err := browser.NewPool(browser.PoolConfig{BrowserPath: path, Size: 1, Headless: true, NoSandbox: true, Network: egress.Config{PrivateOrigins: []string{s.server.URL}}})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			credentials, err := NewFileStore(t.TempDir(), make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			store := &identityPublishFault{Store: identityruntime.NewMemoryStore(), commit: commit}
			key := Key{Site: "fixture", ID: "alice"}
			subject := identityruntime.Subject{Site: key.Site, Kind: "account", Key: key.ID}
			for n := uint64(1); n <= 2; n++ {
				if err = store.AppendProfile(context.Background(), accountIdentityProfile(key.Site, n), n-1); err != nil {
					t.Fatal(err)
				}
			}
			makeManager := func() (*Manager, *identityruntime.Manager) {
				m := NewManager(s.client)
				cfg := s.bridgeConfig(pool, key.ID)
				cfg.Store = credentials
				if err := m.Register(key, cfg); err != nil {
					t.Fatal(err)
				}
				rt, err := identityruntime.NewManager(store, identityruntime.Options{Resolve: m.ResolveIdentity, Verify: m.VerifyIdentity, VerifyTimeout: 30 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				m.SetIdentityManager(rt)
				t.Cleanup(rt.Close)
				return m, rt
			}
			m, rt := makeManager()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			b, err := rt.Stage(ctx, subject, accountIdentityProfile(key.Site, 1).Reference(), 0)
			if err != nil {
				t.Fatal(err)
			}
			b, err = rt.Reverify(ctx, subject, b.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
				t.Fatal(err)
			}
			original, err := credentials.Load(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			b, err = rt.Stage(ctx, subject, accountIdentityProfile(key.Site, 2).Reference(), b.Revision)
			if err != nil {
				t.Fatal(err)
			}
			store.fail = true
			if _, err = rt.Reverify(ctx, subject, b.Revision); !errors.Is(err, identityruntime.ErrUncertain) {
				t.Fatal(err)
			}
			saved, err := credentials.Load(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			candidate := saved.PendingIdentity
			saved.PendingIdentity = nil
			if !reflect.DeepEqual(saved, original) {
				t.Fatal("uncertain binding publication overwrote original durable credentials")
			}
			if candidate == nil || candidate.Identity.Reference.Version != 2 {
				t.Fatal("verified candidate was not durably retained")
			}
			if _, err = rt.Acquire(ctx, subject); !errors.Is(err, identityruntime.ErrUncertain) {
				t.Fatal("uncertain subject admitted use", err)
			}
			store.fail = false
			restarted, restartedRuntime := makeManager()
			binding, err := store.GetBinding(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			expectedLanguage := "fr-FR"
			if !commit {
				if _, err = restartedRuntime.Cancel(ctx, subject, binding.Revision); err != nil {
					t.Fatal(err)
				}
				expectedLanguage = "en-US"
			}
			logins := s.logins.Load()
			if err = restarted.WithBrowser(ctx, key, func(p *rod.Page) error {
				v, err := p.Eval(`() => navigator.language+'|'+firstAccount`)
				if err == nil && v.Value.Str() != expectedLanguage+"|alice" {
					t.Errorf("wrong state selected after uncertain write: %s", v.Value.Str())
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if s.logins.Load() != logins {
				t.Fatal("recovery lost the matching durable credentials")
			}
		})
	}
}
