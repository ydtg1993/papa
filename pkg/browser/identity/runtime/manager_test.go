package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
)

func testProfile(version uint64) identity.Profile {
	return identity.Profile{Site: "fixture", ID: "desktop", Version: version, SchemaVersion: 1, UAPolicy: identity.NativeChromium, Languages: []string{"en-US"}, Locale: "en-US", Timezone: "UTC", Viewport: identity.Size{Width: 800, Height: 600}, Screen: identity.Size{Width: 1280, Height: 720}, DeviceScaleFactor: 1}
}
func testResolve(_ context.Context, _ Subject, p identity.Profile) (*identity.ResolvedProfile, error) {
	return &identity.ResolvedProfile{Reference: p.Reference(), Profile: p.Clone(), Runtime: identity.RuntimeIdentity{Product: "Chrome/130.0.1.1", UserAgent: "fixture-UA", Platform: "Win32"}, UserAgent: "fixture-UA", AcceptLanguage: "en-US", Platform: "Win32", UserAgentMetadata: &proto.EmulationUserAgentMetadata{Brands: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "130"}}}}, nil
}
func fixture(t *testing.T, verify func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error)) (*MemoryStore, *Manager, Subject) {
	t.Helper()
	store := NewMemoryStore()
	for n := uint64(1); n <= 2; n++ {
		if err := store.AppendProfile(context.Background(), testProfile(n), n-1); err != nil {
			t.Fatal(err)
		}
	}
	if verify == nil {
		verify = func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) { return nil, nil }
	}
	m, err := NewManager(store, Options{Resolve: testResolve, Verify: verify, VerifyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return store, m, Subject{Site: "fixture", Kind: "account", Key: "alice"}
}
func activate(t *testing.T, m *Manager, s Subject, version, expected uint64) Binding {
	t.Helper()
	b, err := m.Stage(context.Background(), s, testProfile(version).Reference(), expected)
	if err != nil {
		t.Fatal(err)
	}
	b, err = m.Reverify(context.Background(), s, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIdentityCASAppendAndBindings(t *testing.T) {
	store := NewMemoryStore()
	var success, conflicts atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.AppendProfile(context.Background(), testProfile(1), 0)
			if err == nil {
				success.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 || conflicts.Load() != 23 {
		t.Fatal(success.Load(), conflicts.Load())
	}
	success.Store(0)
	conflicts.Store(0)
	ref := testProfile(1).Reference()
	s := Subject{Site: "fixture", Kind: "account", Key: "alice"}
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.SaveBindingCAS(context.Background(), Binding{Subject: s, Revision: 1, Pending: &ref, State: "pending"}, 0)
			if err == nil {
				success.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 || conflicts.Load() != 23 {
		t.Fatal(success.Load(), conflicts.Load())
	}
	p, _ := store.GetProfile(context.Background(), ref)
	p.Languages[0] = "fr-FR"
	again, _ := store.GetProfile(context.Background(), ref)
	if again.Languages[0] != "en-US" {
		t.Fatal("caller mutated immutable profile")
	}
}

func TestIdentitySubjectRejectsWhitespaceAliases(t *testing.T) {
	for _, field := range []string{"site", "key"} {
		for _, value := range []string{"", " ", "abc ", " abc", "\tabc", "abc\u00a0"} {
			subject := Subject{Site: "fixture", Kind: "session", Key: "business-session-1"}
			if field == "site" {
				subject.Site = value
			} else {
				subject.Key = value
			}
			if subject.Validate() == nil {
				t.Errorf("accepted %s whitespace alias %q", field, value)
			}
		}
	}
	if err := (Subject{Site: "fixture", Kind: "session", Key: "business session 1"}).Validate(); err != nil {
		t.Fatal("interior spaces are not a trailing-space alias", err)
	}
}

func TestIdentityDrainPendingFailureCancelAndRestart(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	store, m, s := fixture(t, func(_ context.Context, _ Subject, r *identity.ResolvedProfile) (func() error, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("synthetic rejected candidate")
		}
		return nil, nil
	})
	b := activate(t, m, s, 1, 0)
	scope, err := m.Acquire(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := m.Stage(context.Background(), s, testProfile(2).Reference(), b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Snapshot().Reference.Version != 1 {
		t.Fatal("staging changed in-flight snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = m.Acquire(ctx, s); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	fail.Store(true)
	done := make(chan error, 1)
	go func() { _, err := m.Reverify(context.Background(), s, pending.Revision); done <- err }()
	select {
	case err := <-done:
		t.Fatal("verification bypassed reference drain", err)
	case <-time.After(20 * time.Millisecond):
	}
	scope.Close()
	scope.Close()
	if err := <-done; !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	failed, _ := store.GetBinding(context.Background(), s)
	if failed.Active.Version != 1 || failed.Pending.Version != 2 || failed.State != "failed" {
		t.Fatal(failed)
	}
	b, err = m.Cancel(context.Background(), s, failed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	before := calls.Load()
	restarted, err := NewManager(store, Options{Resolve: testResolve, Verify: func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) {
		calls.Add(1)
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	newScope, err := restarted.Acquire(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer newScope.Close()
	if newScope.Binding().Revision != b.Revision || newScope.Snapshot().Reference.Version != 1 || calls.Load() != before+1 {
		t.Fatal("restart skipped verification")
	}
}

func TestIdentityExplicitSessionKeyAndScopeCleanup(t *testing.T) {
	store, m, _ := fixture(t, nil)
	s := Subject{Site: "fixture", Kind: "session", Key: "business-session-1"}
	activate(t, m, s, 1, 0)
	if err := m.WithSession(context.Background(), "fixture", "", func(context.Context, *identity.ResolvedProfile) error { return nil }); err == nil {
		t.Fatal("empty persistent session key accepted")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_ = m.WithSession(context.Background(), s.Site, s.Key, func(_ context.Context, r *identity.ResolvedProfile) error {
			if r.Reference.Version != 1 {
				t.Fatal(r)
			}
			panic("synthetic callback")
		})
	}()
	snapshots, _ := m.Snapshots(context.Background(), "fixture")
	if snapshots[0].InUse != 0 {
		t.Fatal("panic leaked identity reference")
	}
	err := m.WithProfile(context.Background(), testProfile(2), func(_ context.Context, r *identity.ResolvedProfile) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	bindings, _ := store.ListBindings(context.Background(), "fixture")
	if len(bindings) != 1 {
		t.Fatal("direct profile created a session binding")
	}
	err = m.WithSession(context.Background(), "fixture", "business-session-2", func(_ context.Context, r *identity.ResolvedProfile) error {
		if r != nil {
			t.Fatal("new session reused another binding")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIdentityCancelResumesVerifiedActiveWithoutDrain(t *testing.T) {
	var calls atomic.Int32
	_, manager, subject := fixture(t, func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) {
		calls.Add(1)
		return nil, nil
	})
	active := activate(t, manager, subject, 1, 0)
	held, err := manager.Acquire(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	pending, err := manager.Stage(context.Background(), subject, testProfile(2).Reference(), active.Revision)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := manager.Cancel(context.Background(), subject, pending.Revision)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	next, err := manager.Acquire(ctx, subject)
	if err != nil {
		t.Fatalf("cancel should resume intake while the old scope remains held: %v", err)
	}
	defer next.Close()
	if held.Snapshot().Reference != testProfile(1).Reference() || next.Snapshot().Reference != testProfile(1).Reference() || next.Binding().Revision != canceled.Revision {
		t.Fatal("cancel replaced active identity or binding revision")
	}
	if calls.Load() != 1 {
		t.Fatalf("cancel unnecessarily reverified the unchanged active identity: %d calls", calls.Load())
	}
	snapshots, err := manager.Snapshots(context.Background(), subject.Site)
	if err != nil || snapshots[0].InUse != 2 || snapshots[0].Verifying {
		t.Fatal(snapshots, err)
	}
}

func TestIdentityCancelAfterRejectedCandidateRetainsActiveVerification(t *testing.T) {
	var calls atomic.Int32
	_, manager, subject := fixture(t, func(_ context.Context, _ Subject, profile *identity.ResolvedProfile) (func() error, error) {
		calls.Add(1)
		if profile.Reference.Version == 2 {
			return nil, errors.New("synthetic rejected candidate")
		}
		return nil, nil
	})
	active := activate(t, manager, subject, 1, 0)
	pending, err := manager.Stage(context.Background(), subject, testProfile(2).Reference(), active.Revision)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := manager.Reverify(context.Background(), subject, pending.Revision)
	if !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	if _, err = manager.Cancel(context.Background(), subject, failed.Revision); err != nil {
		t.Fatal(err)
	}
	next, err := manager.Acquire(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Snapshot().Reference != testProfile(1).Reference() || calls.Load() != 2 {
		t.Fatalf("cancel reverified unchanged active credentials after a rejected detached candidate: %d calls", calls.Load())
	}
}

func TestIdentitySnapshotsDoNotResolveOrAuthenticate(t *testing.T) {
	var calls atomic.Int32
	_, m, s := fixture(t, func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) {
		calls.Add(1)
		return nil, nil
	})
	if _, err := m.Stage(context.Background(), s, testProfile(1).Reference(), 0); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := m.Snapshots(context.Background(), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("GET-like snapshot authenticated")
	}
}

func TestIdentityMissingCorruptAndUncertainRecords(t *testing.T) {
	store, m, s := fixture(t, nil)
	if _, err := m.Stage(context.Background(), s, identity.Reference{Site: "fixture", ID: "missing", Version: 1}, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.bindings[s] = Binding{Subject: s, Revision: 1, State: "active"}
	store.mu.Unlock()
	if _, err := store.GetBinding(context.Background(), s); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestIdentityVerificationPanicAndPublicationFailureStayClosed(t *testing.T) {
	for _, mode := range []string{"verification-panic", "publication-error", "publication-panic"} {
		t.Run(mode, func(t *testing.T) {
			_, m, subject := fixture(t, func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) {
				if mode == "verification-panic" {
					panic("synthetic verifier panic")
				}
				return func() error {
					if mode == "publication-panic" {
						panic("synthetic publication panic")
					}
					return errors.New("synthetic stale candidate")
				}, nil
			})
			binding, err := m.Stage(context.Background(), subject, testProfile(1).Reference(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Reverify(context.Background(), subject, binding.Revision); !errors.Is(err, ErrVerification) {
				t.Fatal(err)
			}
			if _, err = m.Acquire(context.Background(), subject); !errors.Is(err, ErrUncertain) {
				t.Fatal("failed publication admitted new work", err)
			}
			snapshots, err := m.Snapshots(context.Background(), subject.Site)
			if err != nil || snapshots[0].Verifying || snapshots[0].Verified || snapshots[0].InUse != 0 {
				t.Fatal(snapshots, err)
			}
		})
	}
}

func TestIdentityShutdownWaitsForUncooperativeVerifier(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	_, manager, subject := fixture(t, func(ctx context.Context, _ Subject, _ *identity.ResolvedProfile) (func() error, error) {
		entered <- ctx
		<-release
		return nil, nil
	})
	binding, err := manager.Stage(context.Background(), subject, testProfile(1).Reference(), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := manager.Reverify(context.Background(), subject, binding.Revision); done <- err }()
	verifyCtx := <-entered
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if manager.Shutdown(deadline) {
		t.Fatal("shutdown reported drain while verifier ignored cancellation")
	}
	if verifyCtx.Err() == nil {
		t.Fatal("shutdown did not cancel verification context")
	}
	once.Do(func() { close(release) })
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !manager.Shutdown(context.Background()) {
		t.Fatal("completed verifier did not release lifecycle use")
	}
}

type shutdownBlockingStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s *shutdownBlockingStore) SaveBindingCAS(ctx context.Context, binding Binding, expected uint64) error {
	if binding.State == "failed" {
		close(s.entered)
		<-s.release
	}
	return s.Store.SaveBindingCAS(ctx, binding, expected)
}

func TestIdentityShutdownDeadlineDoesNotWaitForSubjectMutex(t *testing.T) {
	store := &shutdownBlockingStore{Store: NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(store.release) })
	if err := store.AppendProfile(context.Background(), testProfile(1), 0); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, Options{Resolve: testResolve, Verify: func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error) {
		return nil, errors.New("synthetic rejected candidate")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	subject := Subject{Site: "fixture", Kind: "account", Key: "alice"}
	binding, err := manager.Stage(context.Background(), subject, testProfile(1).Reference(), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := manager.Reverify(context.Background(), subject, binding.Revision); done <- err }()
	<-store.entered
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if manager.Shutdown(deadline) {
		t.Fatal("shutdown drained while recovery persistence still owns subject lock")
	}
	once.Do(func() { close(store.release) })
	if err = <-done; !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	if !manager.Shutdown(context.Background()) {
		t.Fatal("persistence cleanup leaked lifecycle use")
	}
}

func TestIdentityShutdownCancelsScopesAndExplicitProfileOperations(t *testing.T) {
	_, manager, subject := fixture(t, nil)
	ctx, release, err := manager.Pin(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	entered := make(chan context.Context, 1)
	unblock := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	done := make(chan error, 1)
	go func() {
		done <- manager.WithProfile(context.Background(), testProfile(1), func(ctx context.Context, _ *identity.ResolvedProfile) error {
			entered <- ctx
			<-unblock
			return ctx.Err()
		})
	}()
	profileCtx := <-entered
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if manager.Shutdown(deadline) {
		t.Fatal("shutdown ignored active scope/profile operation")
	}
	if ctx.Err() == nil || profileCtx.Err() == nil {
		t.Fatal("shutdown did not cancel scoped contexts")
	}
	if err = manager.WithProfile(context.Background(), testProfile(1), func(context.Context, *identity.ResolvedProfile) error {
		t.Fatal("closed runtime invoked profile callback")
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, _, err = manager.Pin(context.Background(), subject); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	release()
	once.Do(func() { close(unblock) })
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !manager.Shutdown(context.Background()) {
		t.Fatal("scope/profile completion leaked lifecycle use")
	}
}

func TestIdentityExplicitProfilePanicReleasesShutdownUse(t *testing.T) {
	_, manager, _ := fixture(t, nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected synthetic panic")
			}
		}()
		_ = manager.WithProfile(context.Background(), testProfile(1), func(context.Context, *identity.ResolvedProfile) error { panic("synthetic profile callback") })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !manager.Shutdown(ctx) {
		t.Fatal("profile panic leaked lifecycle use")
	}
}
