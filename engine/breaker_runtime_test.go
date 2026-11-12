package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

type memoryBreakerRepository struct {
	mu   sync.Mutex
	rows map[string]breakerstore.Snapshot
	err  error
}

func (r *memoryBreakerRepository) Save(_ context.Context, l breakerstore.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.rows == nil {
		r.rows = map[string]breakerstore.Snapshot{}
	}
	r.rows[l.Capture.ID] = l.Clean()
	return nil
}
func (r *memoryBreakerRepository) List(_ context.Context, site string, _ int) ([]incident.Lifecycle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	var out []incident.Lifecycle
	for _, l := range r.rows {
		if site == "" || l.Capture.Site == site {
			out = append(out, l.Lifecycle.Clean())
		}
	}
	return out, nil
}
func (r *memoryBreakerRepository) LoadActive(ctx context.Context) ([]incident.Lifecycle, error) {
	all, err := r.List(ctx, "", 100)
	var out []incident.Lifecycle
	for _, l := range all {
		if l.Active() {
			out = append(out, l)
		}
	}
	return out, err
}

func (r *memoryBreakerRepository) LoadRuntime(ctx context.Context) ([]breakerstore.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	var out []breakerstore.Snapshot
	for _, row := range r.rows {
		latest := true
		for _, other := range r.rows {
			if other.Control.Site == row.Control.Site && (other.StartedAt.After(row.StartedAt) || (other.StartedAt.Equal(row.StartedAt) && other.Control.ID > row.Control.ID)) {
				latest = false
			}
		}
		if row.Control.Active() || latest {
			out = append(out, row.Clean())
		}
	}
	return out, nil
}

func newSmartBreakerFixture(t *testing.T, threshold int) (*Engine, *memoryBreakerRepository) {
	t.Helper()
	r := &memoryBreakerRepository{}
	e := &Engine{breakerStore: r, breakerRuntime: true}
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Window: time.Minute, Threshold: threshold, Policy: breaker.Policy{FastThreshold: 3, MixedThreshold: 4}})
	return e, r
}
func smartFailure(e *Engine, id int, stage, code string) {
	e.breakerMu.Lock()
	epoch := e.smartBreakers["synthetic"].epoch
	e.breakerMu.Unlock()
	e.recordSmartOutcome(&Task{ID: id, Site: "synthetic", Stage: stage, breakerRound: "0:0", breakerEpoch: epoch}, core.WithFailureCode(errors.New("synthetic fixture"), "parse", code))
}
func smartSuccess(e *Engine, task *Task) {
	e.breakerMu.Lock()
	task.breakerTrial = e.smartBreakers[task.Site].trial
	task.breakerEpoch = e.smartBreakers[task.Site].epoch
	e.breakerMu.Unlock()
	e.recordSmartOutcome(task, nil)
}
func smartLifecycle(e *Engine) incident.Lifecycle {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if s := e.smartBreakers["synthetic"]; s.event != nil {
		return s.event.reportSnapshot()
	}
	return incident.Lifecycle{}
}
func flushSmart(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.flushBreakerReports(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func expectSmartState(t *testing.T, e *Engine, want string) incident.Lifecycle {
	t.Helper()
	got := smartLifecycle(e)
	if got.State != want {
		t.Fatalf("state=%s want=%s report=%+v", got.State, want, got)
	}
	return got
}

func TestSmartBreakerFastScopeMixedObservationAndHardCeiling(t *testing.T) {
	t.Run("fast local", func(t *testing.T) {
		e, _ := newSmartBreakerFixture(t, 10)
		for id := 1; id <= 3; id++ {
			smartFailure(e, id, "detail", "title_missing")
		}
		l := expectSmartState(t, e, "cooldown")
		if l.Rule != "fast" || l.Stage != "" || !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) || !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) {
			t.Fatalf("local protection: %+v", l)
		}
	})
	t.Run("mixed then hard", func(t *testing.T) {
		e, _ := newSmartBreakerFixture(t, 6)
		for id := 1; id <= 4; id++ {
			smartFailure(e, id, "detail", fmt.Sprint(id))
		}
		l := expectSmartState(t, e, "observing")
		if l.Rule != "mixed" || e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
			t.Fatalf("mixed must observe first: %+v", l)
		}
		flushSmart(t, e)
		done, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"})
		if !ok {
			t.Fatal("observing must allow bounded work")
		}
		if _, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
			t.Fatal("observing concurrency cap lost")
		}
		done()
		for id := 5; id <= 6; id++ {
			smartFailure(e, id, "detail", fmt.Sprint(id))
		}
		l = expectSmartState(t, e, "cooldown")
		if l.Rule != "hard" || l.Stage != "" || !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) {
			t.Fatalf("hard must cover site: %+v", l)
		}
	})
}

func TestSmartBreakerPersistenceFailureNeverReleasesAdmission(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	r.err = errors.New("synthetic store unavailable")
	if err := e.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("store failure swallowed")
	}
	e.tickBreakers(context.Background(), time.Now().Add(time.Minute))
	expectSmartState(t, e, "cooldown")
	if _, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		t.Fatal("admission opened before durable transition")
	}
}

func startSmartProbe(t *testing.T, e *Engine, probe BreakerProbe) {
	t.Helper()
	e.SetBreakerProbe("synthetic", "", probe)
	flushSmart(t, e)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "probing")
	if l.ProbeAttempts != 1 {
		t.Fatalf("probe budget not reserved: %d", l.ProbeAttempts)
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
}

func TestSmartBreakerProbeRequiresSemanticSuccessAndAttemptBudget(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: false}, nil
	})
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "cooldown")
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("HTTP-only success released protection")
	}
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].policy.MaxProbes = 1
	e.breakerMu.Unlock()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "manual")
	if l.ProbeAttempts != 1 || l.RecheckAt.IsZero() {
		t.Fatalf("budget final report: %+v", l)
	}
}

func TestSmartBreakerLateProbeCannotOverrideManualTakeover(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never entered")
	}
	e.setPause(pauseScope{Site: "synthetic"}, pauseState{Source: PauseSourceManual, Reason: "operator takeover"})
	e.tickBreakers(context.Background(), time.Now())
	expectSmartState(t, e, "closed")
	close(release)
	e.tickerWG.Wait()
	expectSmartState(t, e, "closed")
	if st, ok := e.pauseStateOfScope(pauseScope{Site: "synthetic"}); !ok || st.Source != PauseSourceManual {
		t.Fatal("late probe erased manual pause")
	}
}

func TestSmartBreakerSemanticSuccessPersistsBeforeRamping(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "ramping")
	if l.Epoch != 0 || e.smartBreakers["synthetic"].detector.Snapshot(time.Now()).Failures != 1 {
		t.Fatal("probe success cleared the site failure window")
	}
	r.err = errors.New("synthetic write failure")
	if err := e.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected save failure")
	}
	e.tickBreakers(context.Background(), time.Now())
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("unsaved ramp released pause")
	}
	r.err = nil
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	if e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("durable ramp did not release automatic pause")
	}
	smartSuccess(e, &Task{ID: 3, Site: "synthetic", Stage: "detail", breakerEpoch: l.Epoch})
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(time.Minute))
	expectSmartState(t, e, "recovered")
	if _, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		t.Fatal("unsaved recovery released admission")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(time.Minute))
	expectSmartState(t, e, "recovered")
}

func TestSmartBreakerOtherStageFailureInvalidatesSiteRecovery(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 6)
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "title_missing")
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	smartFailure(e, 10, "list", "list_moved_1")
	l := expectSmartState(t, e, "cooldown")
	if l.Stage != "" {
		t.Fatalf("other-stage failure lost site protection: %+v", l)
	}
	for id := 11; id <= 15; id++ {
		smartFailure(e, id, "list", fmt.Sprintf("list_moved_%d", id))
	}
	l = expectSmartState(t, e, "cooldown")
	if l.Rule != "hard" || l.Stage != "" {
		t.Fatalf("hard ceiling failed to broaden scope: %+v", l)
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(time.Second))
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) || !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("escalation lost site protection")
	}
}

func TestSmartBreakerScopeEscalationInvalidatesInFlightStageProbe(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 6)
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "title_missing")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never entered")
	}
	for id := 10; id <= 12; id++ {
		smartFailure(e, id, "list", fmt.Sprintf("list_%d", id))
	}
	l := expectSmartState(t, e, "cooldown")
	if l.Stage != "" || l.Rule != "hard" {
		t.Fatalf("scope did not escalate: %+v", l)
	}
	close(release)
	e.tickerWG.Wait()
	expectSmartState(t, e, "cooldown")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(time.Second))
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) {
		t.Fatal("old stage probe reopened site")
	}
}

func TestSmartBreakerRestartRetainsManualIncident(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	e.breakerMu.Lock()
	e.transitionBreakerLocked(e.smartBreakers["synthetic"], "manual", "fixture manual review", time.Now())
	e.breakerMu.Unlock()
	flushSmart(t, e)
	restarted := &Engine{breakerStore: r}
	restarted.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); restarted.tickerWG.Wait() }()
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	l := expectSmartState(t, restarted, "manual")
	if !restarted.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) || l.RecheckAt.IsZero() {
		t.Fatalf("restart lost protection: %+v", l)
	}
}

func TestSmartBreakerRecoveryDeadlineAndProbeTimeout(t *testing.T) {
	t.Run("recovery deadline", func(t *testing.T) {
		e, _ := newSmartBreakerFixture(t, 1)
		smartFailure(e, 1, "detail", "missing")
		flushSmart(t, e)
		l := smartLifecycle(e)
		e.tickBreakers(context.Background(), l.Deadline)
		l = expectSmartState(t, e, "manual")
		if l.RecheckAt.IsZero() || l.ProbeAttempts != 0 || !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
			t.Fatalf("deadline did not finalize paused report: %+v", l)
		}
	})
	t.Run("bounded probe", func(t *testing.T) {
		e, _ := newSmartBreakerFixture(t, 1)
		smartFailure(e, 1, "detail", "missing")
		e.breakerMu.Lock()
		e.smartBreakers["synthetic"].policy.ProbeTimeout = 5 * time.Millisecond
		e.breakerMu.Unlock()
		startSmartProbe(t, e, func(ctx context.Context, _ BreakerProbeRequest) (BreakerProbeResult, error) {
			<-ctx.Done()
			return BreakerProbeResult{}, ctx.Err()
		})
		e.tickerWG.Wait()
		l := expectSmartState(t, e, "manual")
		if l.RecheckAt.IsZero() || l.ProbeAttempts != 1 || !e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
			t.Fatalf("probe timeout did not finalize paused report: %+v", l)
		}
	})
}
func TestReviewSiteProbeKeepsAllStageFailures(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	other := &Task{ID: 50, Site: "synthetic", Stage: "list", breakerRound: "0:0"}
	done, ok := e.admitBreakerTask(other)
	if !ok {
		t.Fatal("admission")
	}
	smartFailure(e, 49, "list", "separate_failure")
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	e.recordSmartOutcome(other, errors.New("late list failure during site pause"))
	done()
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	if count := e.breakerSum("synthetic"); count != 5 {
		t.Fatalf("partial stage validation cleared failures: %d", count)
	}
	expectSmartState(t, e, "probing")
}

func TestReviewRampDoesNotFinishWithUnsettledTrial(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].policy.Interval = time.Nanosecond
	e.smartBreakers["synthetic"].policy.RampConcurrency = 2
	e.breakerMu.Unlock()
	good := &Task{ID: 2, Site: "synthetic", Stage: "detail", breakerRound: "0:0"}
	done, ok := e.admitBreakerTask(good)
	if !ok {
		t.Fatal("good admission")
	}
	e.recordSmartOutcome(good, nil)
	done()
	flushSmart(t, e)
	slow := &Task{ID: 3, Site: "synthetic", Stage: "detail", breakerRound: "0:0"}
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].nextAdmission = time.Time{}
	e.breakerMu.Unlock()
	release, ok := e.admitBreakerTask(slow)
	if !ok {
		t.Fatal("slow admission")
	}
	defer release()
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	if got := smartLifecycle(e).State; got != "ramping" {
		t.Errorf("declared %s while the current recovery trial remains in flight", got)
	}
	if _, ok := e.admitBreakerTask(&Task{ID: 4, Site: "synthetic", Stage: "detail"}); ok {
		t.Fatal("draining recovery admitted new work")
	}
	e.recordSmartOutcome(slow, errors.New("current recovery trial failed"))
	if got := smartLifecycle(e).State; got != "cooldown" {
		t.Errorf("failed recovery trial was discarded after epoch change: state=%s", got)
	}
}

type reviewInterleavingRepository struct {
	*memoryBreakerRepository
	duringSave func()
}

func (r *reviewInterleavingRepository) Save(ctx context.Context, l breakerstore.Snapshot) error {
	if r.duringSave != nil {
		r.duringSave()
	}
	return r.memoryBreakerRepository.Save(ctx, l)
}

func TestReviewBusyOtherStageMustNotStarveRecoveryBudget(t *testing.T) {
	e, base := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	l := smartLifecycle(e)
	id := 100
	e.breakerStore = &reviewInterleavingRepository{memoryBreakerRepository: base, duringSave: func() {
		id++
		// Reproduce a terminal success landing after snapshot capture, before Save returns.
		smartSuccess(e, &Task{ID: id, Site: "synthetic", Stage: "list", breakerRound: "0:0"})
	}}
	for i := 0; i < 5; i++ {
		flushSmart(t, e)
		e.tickBreakers(context.Background(), l.Deadline.Add(time.Duration(i+1)*time.Second))
	}
	if got := smartLifecycle(e).State; got != "manual" {
		t.Fatalf("healthy unrelated traffic prevents enforcing expired recovery budget: state=%s", got)
	}
}

func TestReviewMixedObservationCanRejectAStaleIncident(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 4; i++ {
		smartFailure(e, i, "detail", fmt.Sprintf("different_%d", i))
	}
	l := expectSmartState(t, e, "observing")
	for i := 10; i <= 60; i++ {
		smartSuccess(e, &Task{ID: i, Site: "synthetic", Stage: "detail", breakerRound: "0:0"})
	}
	for i := 1; i <= 3; i++ {
		smartSuccess(e, &Task{ID: i, Site: "synthetic", Stage: "detail", breakerRound: "verification"})
	}
	flushSmart(t, e)
	// Plenty of successful work and no further failure; short-window failures expired.
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	if e.isPaused(pauseScope{Site: "synthetic"}) {
		t.Fatalf("observation always escalates to site pause: state=%s successes=%d", smartLifecycle(e).State, smartLifecycle(e).Successes)
	}
}
