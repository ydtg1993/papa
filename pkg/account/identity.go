package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

// IdentityState labels the credentials produced by a verified frozen browser
// profile. It contains no observations or credentials and is never an account
// management or pool selection version.
type IdentityState struct {
	Reference     identity.Reference `json:"reference"`
	RuntimeDigest string             `json:"runtime_digest"`
}

func identityMetadata(r *identity.ResolvedProfile) *IdentityState {
	if r == nil {
		return nil
	}
	raw, _ := json.Marshal(r.Runtime)
	sum := sha256.Sum256(raw)
	return &IdentityState{Reference: r.Reference, RuntimeDigest: hex.EncodeToString(sum[:])}
}
func cloneIdentityState(s *IdentityState) *IdentityState {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}
func identityOrSiteFailure(err error) bool {
	var initialization *identity.InitializationError
	return errors.As(err, &initialization) || errors.Is(err, core.ErrSiteChallenge) || errors.Is(err, identity.ErrIncompatible) || errors.Is(err, identity.ErrConflict) || errors.Is(err, identityruntime.ErrVerification) || errors.Is(err, identityruntime.ErrUncertain) || errors.Is(err, identityruntime.ErrClosed)
}
func (m *Manager) SetIdentityManager(v *identityruntime.Manager) {
	m.mu.Lock()
	m.identity = v
	m.mu.Unlock()
}
func (m *Manager) IdentityManager() *identityruntime.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.identity
}
func (m *Manager) PinIdentity(ctx context.Context, key Key) (context.Context, func(), error) {
	v := m.IdentityManager()
	if v == nil {
		return ctx, func() {}, nil
	}
	pinned, release, err := v.Pin(ctx, identityruntime.Subject{Site: key.Site, Kind: "account", Key: key.ID})
	if err != nil {
		return pinned, nil, fmt.Errorf("%w: %w", identityruntime.ErrVerification, err)
	}
	return pinned, release, nil
}
func (e *entry) validateIdentityState(ctx context.Context) error {
	scope, ok := identityruntime.FromContext(ctx)
	if !ok || scope.Snapshot() == nil {
		return nil
	}
	want := identityMetadata(scope.Snapshot())
	e.mu.Lock()
	defer e.mu.Unlock()
	if !reflect.DeepEqual(want, e.identityState) {
		return identityruntime.ErrVerification
	}
	return nil
}

// ResolveIdentity uses this account's actual dedicated pool, without Login,
// Check or credential access. Anonymous callers use their configured pool.
func (m *Manager) ResolveIdentity(ctx context.Context, s identityruntime.Subject, p identity.Profile) (*identity.ResolvedProfile, error) {
	if s.Kind != "account" {
		return nil, ErrUnsupported
	}
	e, err := m.get(Key{Site: s.Site, ID: s.Key})
	if err != nil {
		return nil, err
	}
	if e.config.Browser == nil {
		return nil, ErrUnsupported
	}
	cfg := e.config.Browser
	use := cfg.Pool.WithIsolatedBrowser
	if cfg.Direct {
		use = cfg.Pool.WithDirectIsolatedBrowser
	}
	var result *identity.ResolvedProfile
	err = use(ctx, func(b *browser.Browser) error {
		var err error
		result, err = b.ResolveIdentity(ctx, p.Clone())
		return err
	})
	return result, err
}

// VerifyIdentity authenticates a detached candidate only after old scopes drain.
// A durable candidate is read back before returning its in-memory publication
// callback. Failure leaves the live account and original credentials untouched.
func (m *Manager) VerifyIdentity(ctx context.Context, s identityruntime.Subject, r *identity.ResolvedProfile) (func() error, error) {
	if s.Kind != "account" || r == nil {
		return nil, ErrUnsupported
	}
	key := Key{Site: s.Site, ID: s.Key}
	e, err := m.get(key)
	if err != nil {
		return nil, err
	}
	if e.config.Browser == nil {
		return nil, ErrUnsupported
	}
	e.mu.Lock()
	if e.paused {
		e.mu.Unlock()
		return nil, ErrPaused
	}
	revision, version, browserRevision := e.revision, e.version, e.browserRevision
	cfg := e.config
	b := *cfg.Browser
	b.Options.Headers = maps.Clone(b.Options.Headers)
	b.Options.Identity = r.Clone()
	cfg.Browser = &b
	seed := maps.Clone(e.seed)
	e.mu.Unlock()
	// Keep the main snapshot intact. A pending candidate can only be restored
	// when both its profile reference and actual runtime metadata match exactly.
	original := &StoredState{Version: 1, Key: key}
	if cfg.Store != nil {
		original, err = cfg.Store.Load(ctx, key)
		if errors.Is(err, os.ErrNotExist) {
			original = &StoredState{Version: 1, Key: key}
		} else if err != nil {
			return nil, &Failure{Code: "state-load-failed", Cause: err}
		}
		if original == nil || original.Version != 1 || original.Key != key {
			return nil, &Failure{Code: "invalid-stored-state"}
		}
	}
	selected := *original
	if pending := original.PendingIdentity; pending != nil && reflect.DeepEqual(pending.Identity, identityMetadata(r)) {
		selected.HTTP, selected.Browser, selected.Identity = pending.HTTP, pending.Browser, pending.Identity
	}
	selected.PendingIdentity = nil
	// The candidate has its own locks, slots, cookie jar and browser state.
	candidateManager := NewManager(m.client)
	if err = candidateManager.Register(key, cfg); err != nil {
		return nil, err
	}
	candidate, _ := candidateManager.get(key)
	candidate.autoBridge = e.autoBridge
	candidate.config.Store = &identityLoadStore{state: &selected}
	if err = candidate.load(ctx); err != nil {
		return nil, err
	}
	candidate.mu.Lock()
	candidate.config.Store = nil
	if seed == nil && candidate.current != nil {
		seed = maps.Clone(candidate.current.Values)
	}
	candidate.seed = seed
	if !reflect.DeepEqual(candidate.identityState, identityMetadata(r)) {
		candidate.current = nil
		candidate.pageState = nil
	}
	candidate.identityState = identityMetadata(r)
	candidate.mu.Unlock()
	if cfg.Login != nil {
		authCtx, contextErr := candidate.context(ctx)
		if contextErr != nil {
			return nil, contextErr
		}
		if _, _, err = candidate.ensure(authCtx); err != nil {
			return nil, err
		}
	}
	if err = candidateManager.WithBrowser(ctx, key, func(*rod.Page) error { return nil }); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused || e.revision != revision || e.version != version || e.browserRevision != browserRevision {
		return nil, ErrSuperseded
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if cfg.Store != nil {
		fresh, buildErr := candidate.storedStateLocked()
		if buildErr != nil {
			return nil, buildErr
		}
		// Preserve the live active snapshot if it has advanced since the last
		// persisted envelope. This never installs a candidate under another ID.
		if e.loaded {
			original, buildErr = e.storedStateLocked()
			if buildErr != nil {
				return nil, buildErr
			}
		}
		want := *original
		want.PendingIdentity = &IdentityCandidate{HTTP: fresh.HTTP, Browser: fresh.Browser, Identity: fresh.Identity}
		if err = cfg.Store.Save(ctx, key, &want); err != nil {
			return nil, &Failure{Code: "identity-candidate-save-failed", Cause: err}
		}
		saved, readErr := cfg.Store.Load(ctx, key)
		if readErr != nil || !reflect.DeepEqual(saved, &want) {
			return nil, errors.Join(identityruntime.ErrUncertain, readErr)
		}
	}
	return func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.paused || e.revision != revision || e.version != version || e.browserRevision != browserRevision {
			return ErrSuperseded
		}
		e.current = candidate.current
		e.seed = maps.Clone(candidate.seed)
		e.pageState = candidate.pageState
		e.identityState = cloneIdentityState(candidate.identityState)
		e.loaded = true
		e.lastCheck = candidate.lastCheck
		e.lastLogin = candidate.lastLogin
		e.browserCheck = candidate.browserCheck
		e.failure = nil
		e.retryAt = time.Time{}
		e.browserRevision++
		e.version++
		return nil
	}, nil
}

// Used only while loading the detached candidate; authentication has Store=nil.
type identityLoadStore struct{ state *StoredState }

func (s *identityLoadStore) Load(context.Context, Key) (*StoredState, error) {
	raw, err := json.Marshal(s.state)
	if err != nil {
		return nil, err
	}
	var out StoredState
	err = json.Unmarshal(raw, &out)
	return &out, err
}
func (*identityLoadStore) Save(context.Context, Key, *StoredState) error {
	return errors.New("identity candidate is read-only while loading")
}
func (*identityLoadStore) Delete(context.Context, Key) error {
	return errors.New("identity candidate does not delete credentials")
}

// DiagnosticBrowserConfig returns only the pool's reproducible environment. It
// does not load credentials, resolve a binding, authenticate or launch Chromium.
func (m *Manager) DiagnosticBrowserConfig(key Key) (browser.PoolConfig, []string, error) {
	e, err := m.get(key)
	if err != nil {
		return browser.PoolConfig{}, nil, err
	}
	if e.config.Browser == nil {
		return browser.PoolConfig{}, nil, ErrUnsupported
	}
	cfg, omissions := e.config.Browser.Pool.DiagnosticConfig()
	protected := func(header string) bool {
		h := strings.ToLower(header)
		return h == "user-agent" || h == "accept-language" || strings.HasPrefix(h, "sec-ch-ua")
	}
	if cfg.DefaultHeaders == nil {
		cfg.DefaultHeaders = map[string]string{}
	}
	for h, v := range e.config.Browser.Options.Headers {
		if protected(h) {
			cfg.DefaultHeaders[http.CanonicalHeaderKey(h)] = v
		}
	}
	if e.config.UserAgent != "" {
		cfg.DefaultHeaders["User-Agent"] = e.config.UserAgent
	}
	for _, h := range e.config.Browser.Options.RemoveHeaders {
		if protected(h) {
			return browser.PoolConfig{}, omissions, fmt.Errorf("%w: account removes a protected identity header", identity.ErrConflict)
		}
	}
	if d := e.config.Browser.Options.Device; d != nil {
		device := *d
		device.Capabilities = append([]string(nil), d.Capabilities...)
		cfg.DefaultDevice = &device
	}
	return cfg, omissions, nil
}
