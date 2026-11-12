package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

func registerBreakerFixtureStages(e *Engine, names ...string) {
	e.stages = make(map[string]*stageInfo)
	for _, name := range names {
		e.stages[core.StageKey("synthetic", name)] = &stageInfo{stage: name, config: StageConfig{Site: "synthetic"}}
	}
}

func completeSmartProbeRound(t *testing.T, e *Engine) {
	t.Helper()
	for step := 0; step < 20; step++ {
		l := smartLifecycle(e)
		if l.State != "probing" {
			return
		}
		flushSmart(t, e)
		now := time.Now()
		if now.Before(l.NextProbeAt) {
			now = l.NextProbeAt
		}
		e.tickBreakers(context.Background(), now.Add(time.Millisecond))
		e.tickerWG.Wait()
	}
	t.Fatal("stage verification did not settle")
}

func admitSmartTrial(t *testing.T, e *Engine, id int, stage string) (*Task, func()) {
	t.Helper()
	flushSmart(t, e)
	e.breakers.breakerMu.Lock()
	e.breakers.smartBreakers["synthetic"].nextAdmission = time.Time{}
	e.breakers.breakerMu.Unlock()
	task := &Task{ID: id, Site: "synthetic", Stage: stage, breakerRound: "0:0"}
	done, ok := e.breakers.admitBreakerTask(task)
	if !ok {
		flushSmart(t, e) // persist newly selected serial sample before execution
		done, ok = e.breakers.admitBreakerTask(task)
	}
	if !ok {
		t.Fatalf("controlled admission rejected for %s", stage)
	}
	return task, done
}

func TestSmartBreakerAllStagesDrainThenResetWithReport(t *testing.T) {
	e, repo := newSmartBreakerFixture(t, 100)
	stages := []string{"detail", "episode", "list", "asset"}
	registerBreakerFixtureStages(e, stages...)
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	var checked []string
	startSmartProbe(t, e, func(_ context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		checked = append(checked, req.Stage)
		return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "fixture semantic check"}, nil
	})
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := expectSmartState(t, e, "ramping")
	if len(checked) != 4 || l.ProbeAttempts != 1 || e.breakerSum("synthetic") != 3 {
		t.Fatal("round budget or pre-reset evidence lost")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	for i, stage := range stages {
		task, done := admitSmartTrial(t, e, 10+i, stage)
		e.recordSmartOutcome(task, nil)
		done()
		if i == 0 {
			e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
			expectSmartState(t, e, "ramping")
			if e.breakerSum("synthetic") != 3 {
				t.Fatal("one stage cleared site counters")
			}
		}
	}
	slow, done := admitSmartTrial(t, e, 99, "list")
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "ramping")
	if _, ok := e.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		t.Fatal("draining admitted more work")
	}
	e.recordSmartOutcome(slow, nil)
	done()
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	final := expectSmartState(t, e, "recovered")
	if e.breakerSum("synthetic") != 0 || final.WindowFailures != 3 || final.CountersResetAt.IsZero() {
		t.Fatal("verified reset lost pre-reset report")
	}
	repo.err = errors.New("fixture save failure")
	if err := e.breakers.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected failed final write")
	}
	if _, ok := e.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "list"}); ok {
		t.Fatal("unsaved final state reopened site")
	}
	repo.err = nil
	flushSmart(t, e)
	_, release := admitSmartTrial(t, e, 100, "detail")
	release()
	for _, action := range []string{"cooldown", "stage_probe_started", "stage_probe_passed", "stage_task_success", "waiting_inflight", "recovered"} {
		if !strings.Contains(final.Text(), action) {
			t.Fatalf("operator report missing %s", action)
		}
	}
}

func TestSmartBreakerStageFailureInvalidatesWholeRound(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	startSmartProbe(t, e, func(_ context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: req.Stage == "detail", Detail: "fixture " + req.Stage}, nil
	})
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := expectSmartState(t, e, "cooldown")
	if l.Checks[0].State != "passed" || l.Checks[1].State != "failed" || e.breakerSum("synthetic") != 3 {
		t.Fatal("partial result or failure record lost")
	}
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("passed stage was independently reopened")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "probing")
	if l.ProbeAttempts != 2 || l.Checks[0].State != "probing" || l.Checks[1].State != "pending" {
		t.Fatal("retry reused old passed stages")
	}
}

func TestSmartBreakerMissingStageProbeStopsWholeSite(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	e.SetBreakerProbe("synthetic", "detail", func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	flushSmart(t, e)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	completeSmartProbeRound(t, e)
	l = expectSmartState(t, e, "manual")
	if l.Checks[1].State != "missing" || !strings.Contains(l.Text(), "list") || e.breakerSum("synthetic") != 3 {
		t.Fatal("missing phase silently passed")
	}
}

func TestSmartBreakerOldTaskSuccessCannotProveNewTrial(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for id := 1; id <= 4; id++ {
		smartFailure(e, id, "detail", fmt.Sprint(id))
	}
	for id := 10; id < 14; id++ {
		e.recordSmartOutcome(&Task{ID: id, Site: "synthetic", Stage: "detail", breakerRound: "0:0"}, nil)
	}
	if smartLifecycle(e).Checks[0].TaskSuccesses != 0 {
		t.Fatal("pre-observation task certified new trial")
	}
	flushSmart(t, e)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "observing")
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	expectSmartState(t, e, "manual")
}

func TestSmartBreakerLocalSuccessCannotReopenSite(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) {
		t.Fatal("local fault left the other stage running without site protection")
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	if got := smartLifecycle(e); got.State != "probing" || len(got.Checks) != 2 {
		t.Fatalf("one stage certified the site before all stage checks: state=%s checks=%v", got.State, got.Checks)
	}
	if got := e.breakerSum("synthetic"); got != 3 {
		t.Fatalf("partial check cleared failures: %d", got)
	}
}

func TestSmartBreakerChecksIncludeIdleRegisteredStage(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "never_executed")
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	if got := smartLifecycle(e); len(got.Checks) != 2 || got.Checks[1].Stage != "never_executed" {
		t.Fatalf("idle registered stage absent from recovery: %+v", got.Checks)
	}
}
