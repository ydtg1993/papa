package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

type runtimeBlockingProvider struct {
	entered chan context.Context
	release chan struct{}
}

func (p *runtimeBlockingProvider) Descriptor() onboarding.Descriptor {
	return onboarding.Descriptor{Site: "fixture", Label: "Fixture", Methods: []onboarding.Method{{ID: "password", Kind: "password", Label: "Password"}}}
}
func (p *runtimeBlockingProvider) Config(context.Context, onboarding.Profile, func(context.Context) (map[string]string, error)) (account.Config, error) {
	return account.Config{}, nil
}
func (p *runtimeBlockingProvider) Start(ctx context.Context, _ onboarding.Profile, _ string, _ map[string]string) (onboarding.Flow, error) {
	p.entered <- ctx
	<-p.release // Model a provider which cannot finish immediately on cancellation.
	return nil, ctx.Err()
}
func (p *runtimeBlockingProvider) Verify(context.Context, onboarding.Profile, *account.Credentials) (onboarding.Identity, error) {
	return onboarding.Identity{}, fmt.Errorf("unexpected verification")
}

func newRuntimeOnboarding(t *testing.T, profiles onboarding.Store) (*onboarding.Service, onboarding.Profile, *runtimeBlockingProvider) {
	t.Helper()
	secrets, err := account.NewFileStore(t.TempDir(), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := onboarding.New(onboarding.Options{Manager: account.NewManager(htmlfetch.NewClient(htmlfetch.Config{})), Profiles: profiles, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	p := &runtimeBlockingProvider{entered: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(p.release); service.Close() })
	if err := service.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	profile, err := service.SaveDraft(context.Background(), onboarding.Profile{Key: account.Key{Site: "fixture", ID: "login"}, Label: "Fixture"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return service, profile, p
}

type runtimeBlockingSampler struct {
	entered chan context.Context
	release chan struct{}
}

func (s *runtimeBlockingSampler) Sample(ctx context.Context) (governance.Metrics, error) {
	s.entered <- ctx
	<-s.release
	return governance.Metrics{}, ctx.Err()
}

func waitRuntimeContext(t *testing.T, entered <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-entered:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatal("runtime operation never entered")
		return nil
	}
}

func assertRuntimeCanceled(t *testing.T, name string, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Errorf("%s was not canceled before waiting for other components", name)
	}
}

func TestRuntimeDrainSignalsAllComponentsBeforeWaiting(t *testing.T) {
	a, _ := runtimeTestApp(t)
	service, profile, provider := newRuntimeOnboarding(t, onboarding.NewMemoryStore())
	a.accountOnboarding = service
	if _, err := service.Start(context.Background(), "operator", profile.Key, profile.Revision, "password", nil, false); err != nil {
		t.Fatal(err)
	}
	loginCtx := waitRuntimeContext(t, provider.entered)

	poolEntered, poolRelease := make(chan context.Context, 1), make(chan struct{})
	manager := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	key := account.Key{Site: "fixture", ID: "pool-member"}
	if err := manager.Register(key, account.Config{Origins: []string{"https://fixture.invalid"}, Login: func(ctx context.Context, _ *account.Credentials) error {
		poolEntered <- ctx
		<-poolRelease
		return ctx.Err()
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	pools, err := account.NewPools(context.Background(), manager, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(poolRelease); pools.Close() })
	a.runtimeState.pools = pools
	poolKey := account.PoolKey{Site: "fixture", Name: "fixture"}
	if _, err := pools.Upsert(context.Background(), account.PoolConfig{Key: poolKey, Members: []string{key.ID}}, 0); err != nil {
		t.Fatal(err)
	}
	poolCtx := waitRuntimeContext(t, poolEntered)

	runCtx, cancel := context.WithCancel(context.Background())
	a.runtimeState.cancel = cancel
	sampler := &runtimeBlockingSampler{entered: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(sampler.release); cancel() })
	a.startRuntimeSampler(runCtx, sampler)
	samplerCtx := waitRuntimeContext(t, sampler.entered)
	identityCtx, identityDone, ok := a.identityState.Begin(context.Background())
	if !ok {
		t.Fatal("identity admission initially closed")
	}
	defer identityDone()

	result := make(chan bool, 1)
	go func() { result <- a.drainRuntime(2 * time.Second) }()
	assertRuntimeCanceled(t, "onboarding", loginCtx)
	assertRuntimeCanceled(t, "pool verification", poolCtx)
	assertRuntimeCanceled(t, "sampler", samplerCtx)
	assertRuntimeCanceled(t, "identity", identityCtx)
	if _, done, ok := a.identityState.Begin(context.Background()); ok {
		done()
		t.Error("identity admission remained open")
	}
	if _, err := pools.Acquire(context.Background(), poolKey, account.HTTP); !errors.Is(err, account.ErrPoolClosed) {
		t.Errorf("pool admission remained open: %v", err)
	}
	if _, err := service.Start(context.Background(), "operator", profile.Key, profile.Revision, "password", nil, false); !errors.Is(err, onboarding.ErrGone) {
		t.Errorf("login admission remained open: %v", err)
	}
	select {
	case drained := <-result:
		if drained {
			t.Fatal("blocked runtime components reported drained")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime wait exceeded its deadline")
	}
}

func TestRuntimeDrainTimeoutStillStopsIdentitiesAndCanBeRetried(t *testing.T) {
	service, profile, provider := newRuntimeOnboarding(t, onboarding.NewMemoryStore())
	if _, err := service.Start(context.Background(), "operator", profile.Key, profile.Revision, "password", nil, false); err != nil {
		t.Fatal(err)
	}
	loginCtx := waitRuntimeContext(t, provider.entered)
	a := &App{accountOnboarding: service}
	identityCtx, identityDone, ok := a.identityState.Begin(context.Background())
	if !ok {
		t.Fatal("identity admission initially closed")
	}
	if a.drainRuntime(20 * time.Millisecond) {
		t.Fatal("blocked provider reported drained")
	}
	assertRuntimeCanceled(t, "onboarding", loginCtx)
	assertRuntimeCanceled(t, "identity", identityCtx)
	identityDone()
	provider.release <- struct{}{}
	if !a.drainRuntime(time.Second) {
		t.Fatal("released operations did not drain on retry")
	}
}

func TestRuntimeDrainBoundsSamplerWait(t *testing.T) {
	a, _ := runtimeTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.runtimeState.cancel = cancel
	sampler := &runtimeBlockingSampler{entered: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(sampler.release); cancel() })
	a.startRuntimeSampler(ctx, sampler)
	sampleCtx := waitRuntimeContext(t, sampler.entered)
	result := make(chan bool, 1)
	go func() { result <- a.drainRuntime(20 * time.Millisecond) }()
	select {
	case drained := <-result:
		if drained {
			t.Fatal("blocked sampler reported drained")
		}
	case <-time.After(time.Second):
		t.Fatal("sampler wait ignored runtime drain deadline")
	}
	assertRuntimeCanceled(t, "sampler", sampleCtx)
	sampler.release <- struct{}{}
	if !a.drainRuntime(time.Second) {
		t.Fatal("released sampler did not drain")
	}
}

func TestMessageListenersStopWithAppContext(t *testing.T) {
	a, _ := runtimeTestApp(t)
	a.Engine.InitDownloaders()
	ctx, cancel := context.WithCancel(context.Background())
	a.mdMsgListener(ctx)
	cancel()
	a.Engine.Stop(time.Second)
	if !a.stopMessageListeners(time.Second) {
		t.Fatal("engine or middleware listener survived App cancellation")
	}
	// Engine's permanent Errors channel remains open; stopping App's consumers
	// must not change the Engine channel contract to make the join succeed.
	select {
	case _, ok := <-a.Engine.Errors()[0]:
		if !ok {
			t.Fatal("engine error channel was closed")
		}
	default:
	}
}

type runtimeBlockingLog struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *runtimeBlockingLog) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestMessageListenersWaitForInFlightMiddlewareLog(t *testing.T) {
	a, _ := runtimeTestApp(t)
	manager := proxy.NewManager("\n", time.Hour) // Queue an error without making a request.
	t.Cleanup(manager.Close)
	a.Engine.SetProxy(manager)
	output := &runtimeBlockingLog{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(output.release); a.stopMessageListeners(time.Second) })
	a.Logger.Proxy = quietLogger()
	a.Logger.Proxy.SetOutput(output)
	a.mdMsgListener(context.Background())
	select {
	case <-output.entered:
	case <-time.After(time.Second):
		t.Fatal("middleware listener never began logging")
	}
	if a.stopMessageListeners(20 * time.Millisecond) {
		t.Fatal("active middleware logging reported drained")
	}
	output.release <- struct{}{}
	if !a.stopMessageListeners(time.Second) {
		t.Fatal("middleware listener did not finish after logging returned")
	}
}

type runtimeFailingProfiles struct{ onboarding.Store }

func (s runtimeFailingProfiles) List(context.Context) ([]onboarding.Profile, error) {
	return nil, errors.New("fixture profile load failed")
}

type runtimeBlockingProfiles struct {
	onboarding.Store
	block   atomic.Bool
	entered chan context.Context
	release chan struct{}
}

func (s *runtimeBlockingProfiles) SaveCAS(ctx context.Context, profile onboarding.Profile, revision uint64) error {
	if s.block.Load() {
		s.entered <- ctx
		<-s.release
	}
	return s.Store.SaveCAS(ctx, profile, revision)
}

func TestRuntimeDrainProfileTransactionCannotBlockOtherStops(t *testing.T) {
	profiles := &runtimeBlockingProfiles{Store: onboarding.NewMemoryStore(), entered: make(chan context.Context, 1), release: make(chan struct{})}
	service, profile, _ := newRuntimeOnboarding(t, profiles)
	profiles.block.Store(true)
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		_, _ = service.SaveDraft(context.Background(), profile, profile.Revision)
	}()
	t.Cleanup(func() { close(profiles.release); <-saved })
	saveCtx := waitRuntimeContext(t, profiles.entered)
	a := &App{accountOnboarding: service}
	identityCtx, identityDone, _ := a.identityState.Begin(context.Background())
	defer identityDone()
	result := make(chan bool, 1)
	go func() { result <- a.drainRuntime(20 * time.Millisecond) }()
	select {
	case drained := <-result:
		if drained {
			t.Error("blocked profile save reported drained")
		}
	case <-time.After(time.Second):
		t.Fatal("profile transaction blocked runtime deadline and later stop signals")
	}
	assertRuntimeCanceled(t, "profile persistence", saveCtx)
	assertRuntimeCanceled(t, "identity", identityCtx)
}

func TestShutdownRunRetainsDatabaseForPendingRuntimeStorage(t *testing.T) {
	a, _ := runtimeTestApp(t)
	a.Config.Crawler.StopTimeout = 20 * time.Millisecond
	profiles := &runtimeBlockingProfiles{Store: onboarding.NewMemoryStore(), entered: make(chan context.Context, 1), release: make(chan struct{})}
	service, profile, _ := newRuntimeOnboarding(t, profiles)
	if err := a.UseAccountOnboarding(service); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := a.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	profiles.block.Store(true)
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		_, _ = service.SaveDraft(context.Background(), profile, profile.Revision)
	}()
	t.Cleanup(func() {
		close(profiles.release)
		<-saved
		service.Close()
		_ = sqlDB.Close()
	})
	saveCtx := waitRuntimeContext(t, profiles.entered)
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.shutdownRun(false) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("application shutdown exceeded runtime storage drain budget")
	}
	assertRuntimeCanceled(t, "profile persistence", saveCtx)
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("application closed the database while runtime storage remained active: %v", err)
	}
}

func TestRunInitializationFailureStopsAcquiredRuntime(t *testing.T) {
	for _, failure := range []string{"schema", "onboarding", "existing-pool-schema"} {
		t.Run(failure, func(t *testing.T) {
			a, fake := runtimeTestApp(t)
			var profiles onboarding.Store = onboarding.NewMemoryStore()
			if failure == "onboarding" {
				profiles = runtimeFailingProfiles{profiles}
			} else {
				fake.SetQueryError("account_pool", "fixture schema failure")
			}
			service, profile, _ := newRuntimeOnboarding(t, profiles)
			if err := a.UseAccountOnboarding(service); err != nil {
				t.Fatal(err)
			}
			if failure == "existing-pool-schema" {
				pools, err := account.NewPools(context.Background(), service.Manager(), account.NewMemoryPoolStore(), account.PoolOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pools.Close)
			}
			func() {
				defer func() {
					want := "fixture schema failure"
					if failure == "onboarding" {
						want = "fixture profile load failed"
					}
					if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), want) {
						t.Errorf("initialization error was lost or replaced during cleanup: %v; want %s", recovered, want)
					}
				}()
				a.Run(context.Background())
			}()
			if _, err := service.Start(context.Background(), "operator", profile.Key, profile.Revision, "password", nil, false); !errors.Is(err, onboarding.ErrGone) {
				t.Errorf("startup failure left onboarding admission open: %v", err)
			}
			if _, done, ok := a.identityState.Begin(context.Background()); ok {
				done()
				t.Error("startup failure left identity admission open")
			}
			if service.Manager().Pools() != nil {
				t.Error("startup failure left the installed account coordinator running")
			}
			sqlDB, err := a.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlDB.Ping(); err == nil {
				t.Error("fully drained startup failure retained database")
			}
		})
	}
}
