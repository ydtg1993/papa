package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// A lock regression must terminate its subprocess, not leave blocked goroutines
// behind in the rest of the engine suite.
func breakerReentryChild(t *testing.T) bool {
	t.Helper()
	const key = "PAPA_TEST_BREAKER_REENTRY_CHILD"
	if os.Getenv(key) == t.Name() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), key+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synchronous breaker observer blocked/failed: %v; deadline=%v\n%s", err, ctx.Err(), output)
	}
	return false
}

func TestBreakerEventsPublishPauseBeforeReentrantQueries(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	observed := 0
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		observed++
		status := e.BreakerStatuses()["synthetic"]
		e.GetStageStats()
		if ev.Attrs["action"] == "cooldown" && !status.Paused {
			t.Error("observer saw transition before its pause was published")
		}
	}))
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "fixture")
	}
	if !e.PauseSite("synthetic", "operator") {
		t.Fatal("manual takeover failed")
	}
	if !e.ForceResumeSite("synthetic") {
		t.Fatal("force resume failed")
	}
	if !e.PauseStage("synthetic", "detail", "operator") || !e.ResumeStage("synthetic", "detail") {
		t.Fatal("stage pause/resume failed")
	}
	if observed < 4 {
		t.Fatalf("too few delivered observations: %d", observed)
	}
}

func TestBreakerProbeObserverRetryCannotBeOverwrittenByOldResult(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	s.probeRunning, s.probeExecuting = true, true
	retried := false
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] != "stage_probe_passed" {
			return
		}
		// The user probe has returned; wait for its tracked exit bookkeeping, which
		// must be able to take breakerMu while this observer is running.
		deadline := time.Now().Add(time.Second)
		for {
			e.breakers.breakerMu.Lock()
			executing := s.probeExecuting
			e.breakers.unlockAndEmit()
			if !executing {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("probe exit blocked by observer")
			}
			time.Sleep(time.Millisecond)
		}
		if !e.PauseSite("synthetic", "retry with new budget") || !e.ResumeSite("synthetic") {
			t.Fatal("observer could not request a new round")
		}
		retried = true
	}))
	e.tickerWG.Add(1)
	e.runBreakerProbe(context.Background(), func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil
	}, BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, time.Minute)
	e.tickerWG.Wait()
	if !retried || s.trial != 1 || s.event.ProbeAttempts != 0 || s.event.State != "cooldown" {
		t.Fatalf("old result overwrote retry: retried=%v trial=%d state=%s round=%d", retried, s.trial, s.event.State, s.event.ProbeAttempts)
	}
}

func TestBreakerObserverCanStopAfterCommittedTransition(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	e.loggerSet = testLoggerSet()
	e.pools = newPoolRegistry()
	stopped := false
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		e.BreakerStatuses()
		if ev.Attrs["action"] == "cooldown" && !stopped {
			stopped = true
			if drained, stats := e.Stop(time.Second); !drained || len(stats) != 0 {
				t.Errorf("observer Stop did not drain: %v %+v", drained, stats)
			}
		}
	}))
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "fixture")
	}
	if !stopped || e.ctx.Err() == nil {
		t.Fatal("observer did not stop engine")
	}
}

func TestBreakerQueuedEventsDetachMutableMaps(t *testing.T) {
	b := &breakerCoordinator{}
	attrs := map[string]string{"action": "original"}
	b.events = stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] != "original" {
			t.Fatal("event retained caller map alias")
		}
		ev.Attrs["action"] = "observer"
	})
	b.breakerMu.Lock()
	b.queueEventLocked(events.Event{Attrs: attrs})
	attrs["action"] = "caller"
	b.unlockAndEmit()
	if attrs["action"] != "caller" {
		t.Fatal("observer mutated caller map")
	}
}

func TestBreakerTransactionsNeverInvokeEmitterDirectly(t *testing.T) {
	files, err := filepath.Glob("breaker*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "pause.go")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if (sel.Sel.Name == "Emit" || sel.Sel.Name == "emit") && file != "breaker_events.go" {
				t.Errorf("%s invokes emitter inside breaker/pause component; queue an immutable event instead", file)
			}
			if sel.Sel.Name == "Unlock" {
				if lock, ok := sel.X.(*ast.SelectorExpr); ok && lock.Sel.Name == "breakerMu" && file != "breaker_events.go" {
					t.Errorf("%s bypasses transaction event dispatch", file)
				}
			}
			return true
		})
	}
}

func TestBreakerProbeObserverStopHonorsTrackedDrainDeadline(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	e.loggerSet = testLoggerSet()
	e.pools = newPoolRegistry()
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	s.probeRunning, s.probeExecuting = true, true
	stopped := false
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] == "stage_probe_passed" && !stopped {
			stopped = true
			if drained, _ := e.Stop(25 * time.Millisecond); drained {
				t.Error("Stop falsely drained its still-running probe observer")
			}
		}
	}))
	e.tickerWG.Add(1)
	e.runBreakerProbe(e.ctx, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil
	}, BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, time.Minute)
	e.tickerWG.Wait()
	if !stopped || s.event.State == "ramping" {
		t.Fatal("observer Stop failed or stale probe result enabled ramping")
	}
}

func TestBreakerManualPauseObserverResumeKeepsLatestSiteProjection(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	store := &fakeSiteStore{rows: []core.SiteStat{{Key: "synthetic"}}}
	e.SetSiteStore(store)
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "fixture")
	}
	resumed := false
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] == "manual_pause" && !resumed {
			resumed = true
			if !e.ForceResumeSite("synthetic") {
				t.Error("reentrant force resume failed")
			}
		}
	}))
	if !e.PauseSite("synthetic", "operator") {
		t.Fatal("pause failed")
	}
	if !resumed || e.BreakerStatuses()["synthetic"].Paused || store.snapshot()[0].BreakerPaused {
		t.Fatal("outer pause overwrote observer's later resume")
	}
}

func TestBreakerRestoreObserverStopDoesNotStartMaintenanceAfterDrain(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	first, store := newSmartBreakerFixture(t, 3)
	for id := 1; id <= 3; id++ {
		smartFailure(first, id, "detail", "fixture")
	}
	flushSmart(t, first)
	e, _ := newSmartBreakerFixture(t, 3)
	e.breakers.breakerRuntime = false
	e.breakers.breakerStore = store
	e.ctx, e.cancel = context.WithCancel(context.Background())
	defer e.cancel()
	e.loggerSet = testLoggerSet()
	e.pools = newPoolRegistry()
	stopped := false
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Type == events.TypeBreakerTransition && !stopped {
			stopped = true
			if drained, _ := e.Stop(time.Second); !drained {
				t.Error("restore observer could not drain before maintenance starts")
			}
		}
	}))
	if err := e.InitBreakerRuntime(context.Background()); err != context.Canceled {
		t.Fatalf("startup continued after observer stop: %v", err)
	}
	e.tickerWG.Wait()
	if !stopped {
		t.Fatal("restoration event was not emitted")
	}
}

func TestBreakerProbeLocalCancellationAfterMilestonePreservesOriginalDecision(t *testing.T) {
	b := &breakerCoordinator{}
	b.SetSiteBreaker("fixture", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	s := b.smartBreakers["fixture"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "fixture", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.events = stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] == "stage_probe_passed" {
			cancel()
		}
	})
	b.breakerMu.Lock()
	token, reason := b.settleStageProbeLocked(BreakerProbeRequest{Site: "fixture", Stage: "detail"}, "incident", 1, BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil, probeCtx)
	b.unlockAndEmit()
	if token == nil || reason != "" || probeCtx.Err() == nil {
		t.Fatal("fixture did not reach local cancellation after successful milestone")
	}
	b.breakerMu.Lock()
	b.finishStageProbeLocked(token, context.Background())
	b.unlockAndEmit()
	if s.event.State != "ramping" {
		t.Fatalf("new phase boundary changed post-milestone local cancellation semantics: %s", s.event.State)
	}
}

func TestBreakerProbeObserverPanicDoesNotStrandSettlementToken(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}, {Stage: "catalog", State: "pending", RequiredProbes: 1}}}}
	s.probeRunning, s.probeExecuting = true, true
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Attrs["action"] == "stage_probe_passed" {
			panic("observer panic")
		}
	}))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		e.tickerWG.Add(1)
		e.runBreakerProbe(context.Background(), func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
			return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil
		}, BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, time.Minute)
	}()
	e.tickerWG.Wait()
	if recovered != "observer panic" || s.probeSettlement != nil {
		t.Fatalf("panic left settlement reserved: panic=%v token=%v", recovered, s.probeSettlement)
	}
	e.breakers.breakerMu.Lock()
	e.breakers.selectStageProbeLocked(s, s.event.NextProbeAt.Add(time.Second), time.Time{})
	e.breakers.unlockAndEmit()
	if s.event.Checks[1].State != "probing" {
		t.Fatalf("remaining stage blocked after recovered observer panic: %s", s.event.Checks[1].State)
	}
}
