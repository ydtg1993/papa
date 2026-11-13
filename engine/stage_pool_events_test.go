package engine

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

type stagePoolEventFunc func(events.Event)

func (f stagePoolEventFunc) Emit(ev events.Event) { f(ev) }

func poolEventFixture(t *testing.T) *Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Engine{ctx: ctx, cancel: cancel, cfg: &config.Config{}, loggerSet: testLoggerSet(), pools: newPoolRegistry(), stages: map[string]*stageInfo{
		"fixture:detail": {stage: "detail", config: StageConfig{Site: "fixture", WorkerCount: 1, QueueSize: 1}},
		"fixture:other":  {stage: "other", config: StageConfig{Site: "fixture", WorkerCount: 1, QueueSize: 1}},
	}}
}

func TestStagePoolCreatedEmitterCanStopEngine(t *testing.T) {
	// Regressions deadlock the callback's goroutine. Isolate this test so a failed
	// deadline kills the child instead of leaking blocked goroutines in the suite.
	const childEnv = "PAPA_TEST_POOL_CREATED_STOP_CHILD"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStagePoolCreatedEmitterCanStopEngine$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("created callback cannot synchronously stop engine: %v; context=%v\n%s", err, ctx.Err(), output)
		}
		return
	}
	e := poolEventFixture(t)
	created := 0
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Type != events.TypeStagePool || ev.Source != "created" {
			return
		}
		created++
		if e.pools.Get("fixture:detail") == nil {
			t.Error("event published before pool registration")
		}
		if drained, stats := e.Stop(time.Second); !drained || len(stats) != 0 {
			t.Errorf("callback Stop did not drain registered pool: %v %+v", drained, stats)
		}
	}))
	if e.getOrCreatePool("fixture:detail") == nil {
		t.Fatal("creator lost its registered pool")
	}
	drainTestEvents(t, e)
	if !e.pools.IsStopped() {
		t.Fatal("callback did not stop registry")
	}
	if e.getOrCreatePool("fixture:other") != nil {
		t.Fatal("shutdown permitted a new pool")
	}
	if created != 1 {
		t.Fatalf("creation events=%d want 1", created)
	}
}

func TestStagePoolConcurrentLookupEmitsCreationOnce(t *testing.T) {
	e := poolEventFixture(t)
	defer e.Stop(time.Second)
	collector := &collectorEmitter{}
	e.SetEventsEmitter(collector)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.getOrCreatePool("fixture:detail") == nil {
				t.Error("registered stage returned nil pool")
			}
		}()
	}
	wg.Wait()
	drainTestEvents(t, e)
	count := 0
	for _, ev := range collector.snapshot() {
		if ev.Type == events.TypeStagePool && ev.Source == "created" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent cache hits emitted %d creation events", count)
	}
}
