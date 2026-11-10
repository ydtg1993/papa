package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestIdentityEngineAdmissionWaitAndRelease(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	key := account.Key{Site: "fixture", ID: "alice"}
	if err := m.Register(key, account.Config{Origins: []string{"https://example.invalid"}, CheckInterval: time.Hour, Login: func(context.Context, *account.Credentials) error { return nil }, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	pools, err := account.NewPools(context.Background(), m, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	pk := account.PoolKey{Site: key.Site, Name: "members"}
	if _, err = pools.Upsert(context.Background(), account.PoolConfig{Key: pk, Members: []string{key.ID}}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = pools.Acquire(ctx, pk, account.HTTP); err != nil {
		t.Fatal(err)
	}
	profile := identity.Profile{Site: key.Site, ID: "desktop", Version: 1, SchemaVersion: 1, UAPolicy: identity.NativeChromium, Languages: []string{"en-US"}, Locale: "en-US", Timezone: "UTC", Viewport: identity.Size{Width: 800, Height: 600}, Screen: identity.Size{Width: 1280, Height: 720}, DeviceScaleFactor: 1}
	store := identityruntime.NewMemoryStore()
	if err = store.AppendProfile(ctx, profile, 0); err != nil {
		t.Fatal(err)
	}
	rt, err := identityruntime.NewManager(store, identityruntime.Options{Resolve: func(_ context.Context, _ identityruntime.Subject, p identity.Profile) (*identity.ResolvedProfile, error) {
		return &identity.ResolvedProfile{Reference: p.Reference(), Profile: p, Runtime: identity.RuntimeIdentity{UserAgent: "fixture", Platform: "Win32"}, UserAgent: "fixture", Platform: "Win32", AcceptLanguage: "en-US", UserAgentMetadata: &proto.EmulationUserAgentMetadata{Brands: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "130"}}}}, nil
	}, Verify: func(context.Context, identityruntime.Subject, *identity.ResolvedProfile) (func() error, error) {
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	subject := identityruntime.Subject{Site: key.Site, Kind: "account", Key: key.ID}
	b, err := rt.Stage(ctx, subject, profile.Reference(), 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err = rt.Reverify(ctx, subject, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdentityManager(rt)
	e.SetIdentityManager(rt)
	e.SetAccountManager(m)
	cfg := governance.DefaultConfig()
	cfg.Resources.Enabled = true
	cfg.Resources.TaskMax = 1
	cfg.Defaults.Request.Enabled = true
	controller, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(controller, nil)
	// The task holds exactly one profile use even if it performs nested account calls.
	prepared, release, err := e.prepareRuntimeTask(ctx, &Task{Site: key.Site, Stage: "detail", AccountPool: pk.Name}, 1)
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := identityruntime.FromContext(prepared)
	if !ok || scope.Snapshot().Reference != profile.Reference() {
		t.Fatal("task did not pin profile")
	}
	snapshots, _ := rt.Snapshots(ctx, key.Site)
	if snapshots[0].InUse != 1 {
		t.Fatal(snapshots)
	}
	release()
	release()
	snapshots, _ = rt.Snapshots(ctx, key.Site)
	if snapshots[0].InUse != 0 {
		t.Fatal("task release leaked profile reference")
	}
	// A pending binding wait precedes global admission and remains cancellable.
	if _, err = rt.Stage(ctx, subject, profile.Reference(), b.Revision); err != nil {
		t.Fatal(err)
	}
	blockedCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	_, _, err = e.prepareRuntimeTask(blockedCtx, &Task{Site: key.Site, Stage: "detail", AccountPool: pk.Name}, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if state := controller.Snapshot().Task; state.Inflight != 0 || state.Waiting != 0 {
		t.Fatal("binding wait reserved global capacity", state)
	}
	// Anonymous runtime ownership does not require a configured account manager.
	e.SetAccountManager(nil)
	if e.IdentityManager() != rt {
		t.Fatal("anonymous identity runtime lost without accounts")
	}
}
