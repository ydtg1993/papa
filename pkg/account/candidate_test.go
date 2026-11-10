package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func candidateToken(token string) *Credentials {
	return &Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{"Authorization": token}}
}

func TestPublishCandidateCheckAndSaveFailureKeepOldCredentials(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	key := Key{"s", "a"}
	e, _ := m.get(key)
	if _, err := m.PublishCandidate(context.Background(), key, 0, candidateToken("old")); err != nil {
		t.Fatal(err)
	}
	version, _ := m.Version(key)
	e.config.Check = func(_ context.Context, c *Credentials) (bool, error) {
		return c.Headers["Authorization"] != "rejected", nil
	}
	if _, err := m.PublishCandidate(context.Background(), key, version, candidateToken("rejected")); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	for _, mode := range []string{"error", "panic"} {
		e.config.Store = &faultStore{mode: mode}
		if _, err := m.PublishCandidate(context.Background(), key, version, candidateToken("new")); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("save failure not sanitized: %v", err)
		}
		e.mu.Lock()
		if e.current.Headers["Authorization"] != "old" || e.version != version {
			e.mu.Unlock()
			t.Fatal("failed publication replaced old credentials")
		}
		e.mu.Unlock()
	}
}

func TestRegisterCandidateDoesNotPublishRejectedOrDuplicateAccount(t *testing.T) {
	m, srv, _ := poolManager(t, nil, "existing")
	cfg := Config{Origins: []string{srv.URL}, Login: func(context.Context, *Credentials) error { return errors.New("must not login") }, Check: func(_ context.Context, c *Credentials) (bool, error) {
		return c.Headers["Authorization"] == "valid", nil
	}}
	key := Key{"s", "candidate"}
	if err := m.RegisterCandidate(context.Background(), key, cfg, candidateToken("bad")); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	if _, err := m.Version(key); !errors.Is(err, ErrUnknown) {
		t.Fatal("rejected candidate registered")
	}
	if err := m.RegisterCandidate(context.Background(), key, cfg, candidateToken("valid")); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterCandidate(context.Background(), key, cfg, candidateToken("valid")); !errors.Is(err, ErrPoolConflict) {
		t.Fatalf("duplicate candidate: %v", err)
	}
}

func TestCandidatePublishedPreservesDisabledAndRejectsOldGeneration(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	key := createPool(t, p, "one", "a")
	a := acquirePool(t, p, key)
	p.ReportFailure(a, NeedsVerify, 0)
	if err := p.CandidatePublished(context.Background(), a.Account, a.CredentialVersion); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old credential cleared suspicion: %v", err)
	}
	_, revision, _ := p.Policy(a.Account)
	if err := p.SetDisabled(context.Background(), a.Account, true, revision); err != nil {
		t.Fatal(err)
	}
	v, err := m.PublishCandidate(context.Background(), a.Account, a.CredentialVersion, candidateToken("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.CandidatePublished(context.Background(), a.Account, v); err != nil {
		t.Fatal(err)
	}
	s := managementFor(t, p, key, a.Account)
	if !s.Disabled || s.Suspicious || s.Quarantined {
		t.Fatalf("candidate publication changed disabled: %+v", s)
	}
}

type cancelAfterCandidateSave struct {
	Store
	cancel context.CancelFunc
}

func (s *cancelAfterCandidateSave) Save(ctx context.Context, key Key, state *StoredState) error {
	err := s.Store.Save(ctx, key, state)
	if err == nil {
		s.cancel()
	}
	return err
}

func TestCandidateCancellationAfterSuccessfulSaveKeepsCommittedMemory(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	key := Key{"s", "a"}
	e, _ := m.get(key)
	if _, err := m.PublishCandidate(context.Background(), key, 0, candidateToken("old")); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFileStore(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.config.Store = &cancelAfterCandidateSave{Store: fs, cancel: cancel}
	v, err := m.PublishCandidate(ctx, key, 1, candidateToken("new"))
	if err != nil {
		t.Fatal("committed save falsely reported rollback", err)
	}
	if ctx.Err() == nil {
		t.Fatal("fixture did not cancel")
	}
	stored, err := fs.Load(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if stored.Generation != v || e.version != v || e.current.Headers["Authorization"] != "new" || stored.HTTP.Headers["Authorization"] != "new" {
		t.Fatal("cancellation split committed and live credential state")
	}
}

func TestHTTPCandidatePreservesBrowserButCannotClaimBrowserRecovery(t *testing.T) {
	m, srv, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	pool := createPool(t, p, "one", "a")
	a := acquirePool(t, p, pool)
	e, _ := m.get(a.Account)
	e.mu.Lock()
	e.pageState = &browser.PageState{Version: 1, Origin: srv.URL}
	e.browserCheck = time.Now()
	e.mu.Unlock()
	p.mu.Lock()
	managed := p.accounts[a.Account]
	managed.state.Suspicious = true
	managed.state.CheckTransport = Browser
	managed.state.CredentialVersion = a.CredentialVersion
	p.mu.Unlock()
	v, err := m.PublishCandidate(context.Background(), a.Account, a.CredentialVersion, candidateToken("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	retained := e.pageState != nil
	e.mu.Unlock()
	if !retained {
		t.Fatal("HTTP candidate erased unrelated browser state")
	}
	if err = p.CandidatePublished(context.Background(), a.Account, v); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("HTTP evidence cleared browser exclusion: %v", err)
	}
}

func TestCandidateIdentityPinCoversCheckAndPublication(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	e, _ := m.get(Key{"s", "a"})
	profile := accountIdentityProfile("s", 1)
	store := identityruntime.NewMemoryStore()
	if err := store.AppendProfile(context.Background(), profile, 0); err != nil {
		t.Fatal(err)
	}
	runtime, err := identityruntime.NewManager(store, identityruntime.Options{Resolve: func(_ context.Context, _ identityruntime.Subject, p identity.Profile) (*identity.ResolvedProfile, error) {
		return &identity.ResolvedProfile{Reference: p.Reference(), Profile: p.Clone(), Runtime: identity.RuntimeIdentity{Product: "Chrome/130.0.1.1", UserAgent: "fixture-UA", Platform: "Win32"}, UserAgent: "fixture-UA", AcceptLanguage: "en-US", Platform: "Win32", UserAgentMetadata: &proto.EmulationUserAgentMetadata{Brands: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "130"}}}}, nil
	}, Verify: func(context.Context, identityruntime.Subject, *identity.ResolvedProfile) (func() error, error) {
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	m.SetIdentityManager(runtime)
	for _, key := range []Key{{"s", "a"}, {"s", "new"}} {
		subject := identityruntime.Subject{Site: key.Site, Kind: "account", Key: key.ID}
		binding, err := runtime.Stage(context.Background(), subject, profile.Reference(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = runtime.Reverify(context.Background(), subject, binding.Revision); err != nil {
			t.Fatal(err)
		}
	}
	cfg := e.config
	cfg.Check = func(ctx context.Context, _ *Credentials) (bool, error) {
		scope, ok := identityruntime.FromContext(ctx)
		if !ok || scope.Snapshot() == nil || scope.Snapshot().Reference != profile.Reference() {
			return false, errors.New("candidate missing frozen identity")
		}
		return true, nil
	}
	e.config.Check = cfg.Check
	if _, err = m.PublishCandidate(context.Background(), Key{"s", "a"}, 0, candidateToken("fresh")); err != nil {
		t.Fatal(err)
	}
	if err = m.RegisterCandidate(context.Background(), Key{"s", "new"}, cfg, candidateToken("fresh")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{{"s", "a"}, {"s", "new"}} {
		ctx, release, err := m.PinIdentity(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		entry, _ := m.get(key)
		err = entry.validateIdentityState(ctx)
		release()
		if err != nil {
			t.Fatalf("candidate omitted identity metadata: %v", err)
		}
	}
}
