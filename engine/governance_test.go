package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"testing"
	"time"
)

func TestRuntimePrecedenceCapacityAndRandomDelay(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	if err := e.AddStage("stub", StageConfig{Site: "stub", WorkerCount: 4, QueueSize: 8, Delay: config.DurationRange{Min: time.Second, Max: 2 * time.Second}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	fileRate, dbRate := 2.0, 4.0
	fileLimit, dbLimit := 3, 2
	zero := time.Duration(0)
	e.cfg.Runtime = config.RuntimeConfig{Defaults: governance.PolicyPatch{Request: &governance.RequestPatch{RequestsPerSecond: &fileRate}}, Sites: map[string]governance.PolicyPatch{"stub": {Stages: map[string]governance.StagePatch{"stub": {MaxConcurrent: &fileLimit}}}}}
	override := config.RuntimeConfig{Defaults: governance.PolicyPatch{Request: &governance.RequestPatch{RequestsPerSecond: &dbRate}}, Sites: map[string]governance.PolicyPatch{"stub": {Stages: map[string]governance.StagePatch{"stub": {MaxConcurrent: &dbLimit, Delay: &zero}}}}}
	cfg, delays, err := e.ResolveRuntime(override)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Effective("stub")
	if p.Request.RequestsPerSecond != 4 || p.Stages["stub"].MaxConcurrent != 2 {
		t.Fatal("precedence lost", p)
	}
	c, _ := governance.New(cfg)
	e.InstallGovernance(c, delays)
	task := &Task{Site: "stub", Stage: "stub"}
	if e.taskDelay(task, time.Second) != 0 {
		t.Fatal("explicit zero delay lost")
	}
	cfg, delays, err = e.ResolveRuntime(config.RuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(c, delays)
	if e.taskDelay(task, 1500*time.Millisecond) != 1500*time.Millisecond {
		t.Fatal("random declaration replaced by synthetic zero")
	}
	bad := 5
	override.Sites["stub"] = governance.PolicyPatch{Stages: map[string]governance.StagePatch{"stub": {MaxConcurrent: &bad}}}
	if _, _, err = e.ResolveRuntime(override); err == nil {
		t.Fatal("exceeded physical workers")
	}
}

func TestRuntimeAccountFailureEndsTaskWithoutWaitingForReplacement(t *testing.T) {
	e, _, task, eventsOut := persistenceFixture(t)
	manager := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	allowBackup := make(chan struct{})
	defer close(allowBackup)
	for _, id := range []string{"first", "backup"} {
		id := id
		err := manager.Register(account.Key{Site: task.Site, ID: id}, account.Config{Origins: []string{"https://fixture.example"}, CheckInterval: time.Hour, Login: func(ctx context.Context, c *account.Credentials) error {
			if id == "backup" {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-allowBackup:
				}
			}
			return nil
		}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }})
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := account.NewPools(context.Background(), manager, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e.SetAccountManager(manager)
	key := account.PoolKey{Site: task.Site, Name: "primary"}
	if _, err = p.Upsert(context.Background(), account.PoolConfig{Key: key, Members: []string{"first", "backup"}}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = p.Acquire(ctx, key, account.HTTP); err != nil {
		t.Fatal(err)
	}
	task.AccountPool = key.Name
	info := e.stages[stageKeyOf(task)]
	info.config.MaxAttempts = 3
	info.config.Backoff = time.Hour
	calls := 0
	info.fetcher = stageRunVersionFetcher(func(ctx context.Context, _ *Task, _ *Engine) error {
		calls++
		a, ok := account.AttemptFromContext(ctx)
		if !ok || a.Account.ID != "first" {
			t.Fatal("missing fixed attempt identity")
		}
		return account.ErrExpired
	})
	start := time.Now()
	err = e.stageHandler(info)(ctx, task)
	if !errors.Is(err, account.ErrExpired) || calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("failed task waited/retried: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
	}
	if countTaskEvents(eventsOut, events.TypeTaskFailed) != 1 {
		t.Fatal("failed task terminal event missing")
	}
	snapshot, _ := p.Get(key)
	for _, m := range snapshot.Accounts {
		if m.Key.ID == "first" && !m.Management.Quarantined {
			t.Fatal("failure did not notify independent scheduler")
		}
	}
}

func TestRuntimeGlobalAdmissionWaitLeavesTaskPendingOnCancel(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	cfg := governance.DefaultConfig()
	cfg.Resources.Enabled = true
	cfg.Resources.TaskMax = 1
	c, _ := governance.New(cfg)
	e.InstallGovernance(c, nil)
	held, err := c.Acquire(context.Background(), "other", governance.ClassTask)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	e.stages[stageKeyOf(task)].config.WorkerCount = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.stageHandler(e.stages[stageKeyOf(task)])(ctx, task) }()
	deadline := time.After(2 * time.Second)
	for c.Snapshot().Task.Waiting == 0 {
		select {
		case <-deadline:
			t.Fatal("task did not wait")
		case <-time.After(time.Millisecond):
		}
	}
	if len(claimStatements(f)) != 0 {
		t.Fatal("waiting task claimed early")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(claimStatements(f)) != 0 {
		t.Fatal("cancelled pending task claimed")
	}
}

func TestRuntimePreparationDoesNotBindNewPoolToPersistedAnonymousTask(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	cfg := governance.DefaultConfig()
	cfg.Defaults.Pool = "new-default"
	c, _ := governance.New(cfg)
	e.InstallGovernance(c, nil)
	ctx, release, err := e.prepareRuntimeTask(context.Background(), &Task{ID: 42, Site: "stub", Stage: "stub"}, 1)
	if err != nil {
		t.Fatal("legacy task rebound", err)
	}
	release()
	if governance.SiteFromContext(ctx) != "stub" {
		t.Fatal("missing request scope")
	}
}

func TestRuntimeExplicitEmptyStagePoolOverridesSiteDefault(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	if err := e.AddStage("stub", StageConfig{Site: "stub", WorkerCount: 1, QueueSize: 2}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pool, empty := "members", ""
	e.cfg.Runtime.Defaults.Pool = &pool
	cfg, delays, err := e.ResolveRuntime(config.RuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := governance.New(cfg)
	e.InstallGovernance(c, delays)
	if got := e.resolveTaskPool(&Task{Site: "stub", Stage: "stub"}); got != pool {
		t.Fatal("stage did not inherit", got)
	}
	patch := config.RuntimeConfig{Sites: map[string]governance.PolicyPatch{"stub": {Stages: map[string]governance.StagePatch{"stub": {Pool: &empty}}}}}
	cfg, delays, err = e.ResolveRuntime(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Update(cfg); err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(c, delays)
	if got := e.resolveTaskPool(&Task{Site: "stub", Stage: "stub"}); got != "" {
		t.Fatal("explicit empty stage pool ignored", got)
	}
}

func TestRuntimePauseDuringAdmissionDoesNotClaimTask(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	cfg := governance.DefaultConfig()
	cfg.Resources.Enabled = true
	cfg.Resources.TaskMax = 1
	c, _ := governance.New(cfg)
	e.InstallGovernance(c, nil)
	e.stages[stageKeyOf(task)].config.WorkerCount = 1
	held, err := c.Acquire(context.Background(), "other", governance.ClassTask)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.stageHandler(e.stages[stageKeyOf(task)])(ctx, task) }()
	for c.Snapshot().Task.Waiting == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("no admission waiter")
		case <-time.After(time.Millisecond):
		}
	}
	e.setPause(pauseScope{Site: task.Site}, pauseState{Source: PauseSourceManual, Reason: "fixture", At: time.Now()})
	held()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if len(claimStatements(f)) != 0 || c.Snapshot().Task.Inflight != 0 {
		t.Fatal("pause during wait did not preserve pending/free quota")
	}
}
