package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
)

// Mirrors a reserved job for tests of the runner or settlement boundary that
// construct their probing event directly instead of going through the ticker.
func fixtureBreakerProbeJob(s *smartBreaker, probe BreakerProbe) *breakerProbeJob {
	job := &breakerProbeJob{
		state: s, probe: probe,
		request: BreakerProbeRequest{Site: s.event.Site, Stage: s.event.Checks[0].Stage},
		id:      s.event.ID, round: s.event.ProbeAttempts, timeout: time.Minute,
	}
	s.probeJob = job
	s.probeRunning, s.probeExecuting = true, true
	return job
}

func TestBreakerProbeForceResumeNewIncidentDiscardsOldResultAndRecovers(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		e.tickerWG.Wait()
	})
	var calls atomic.Int32
	startSmartProbe(t, e, func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return completeReadOnlyProbe(ctx, req)
	})
	awaitBreakerObservation(t, entered)
	oldID := smartLifecycle(e).Capture.ID
	if !e.ForceResumeSite("synthetic") {
		t.Fatal("force resume rejected")
	}
	flushSmart(t, e)
	expectSmartState(t, e, "closed")
	task := &Task{ID: 10001, Site: "synthetic", Stage: "detail", breakerRound: "0:0"}
	done, ok := e.breakers.admitBreakerTask(task)
	if !ok {
		t.Fatal("normal task rejected after durable force closure")
	}
	e.recordSmartOutcome(task, core.WithFailureCode(errors.New("new normal task failure"), "parse", "missing"))
	done()
	next := expectSmartState(t, e, "cooldown")
	if next.Capture.ID == oldID {
		t.Fatal("new normal failure did not create a fresh incident")
	}
	flushSmart(t, e)
	for i := 0; i < 3; i++ {
		e.tickBreakers(context.Background(), next.NextProbeAt.Add(time.Millisecond))
	}
	if calls.Load() != 1 {
		t.Fatal("new incident overlapped the still executing old probe")
	}
	releaseOnce.Do(func() { close(release) })
	e.tickerWG.Wait()
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	running, executing, owner := s.probeRunning, s.probeExecuting, s.probeJob
	e.breakers.breakerMu.Unlock()
	if running || executing || owner != nil {
		t.Fatalf("completed old probe retained ownership: running=%v executing=%v owner=%p", running, executing, owner)
	}
	next = expectSmartState(t, e, "cooldown")
	if next.Checks[0].ProbeSuccesses != 0 || next.Clean().Checks[0].Verified {
		t.Fatal("old success supplied proof for the new incident")
	}
	e.tickBreakers(context.Background(), next.NextProbeAt.Add(time.Millisecond))
	expectSmartState(t, e, "probing")
	completeSmartProbeRound(t, e)
	next = expectSmartState(t, e, "ramping")
	if calls.Load() != 3 || next.Checks[0].ProbeSuccesses != 2 || !next.Clean().Checks[0].Verified {
		t.Fatalf("new incident did not obtain independent proof: calls=%d checks=%+v", calls.Load(), next.Checks)
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), next.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "recovered")
	flushSmart(t, e)
	done, ok = e.breakers.admitBreakerTask(&Task{ID: 10002, Site: "synthetic", Stage: "detail"})
	if !ok {
		t.Fatal("recovered incident did not resume normal admission")
	}
	done()
}

func TestBreakerProbeTimeoutKeepsExecutionReservedUntilExit(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 3)
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		e.tickerWG.Wait()
	})
	job := fixtureBreakerProbeJob(s, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release // Deliberately ignore the expired probe context.
		return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "late success"}, nil
	})
	job.timeout = 5 * time.Millisecond
	e.tickerWG.Add(1)
	go func() {
		e.runBreakerProbe(context.Background(), job)
		close(finished)
	}()
	awaitBreakerObservation(t, entered)
	awaitBreakerObservation(t, finished)
	e.breakers.breakerMu.Lock()
	running, executing, owner := s.probeRunning, s.probeExecuting, s.probeJob
	e.breakers.breakerMu.Unlock()
	if running || !executing || owner != job {
		t.Fatalf("timeout released live extension: running=%v executing=%v owner=%p", running, executing, owner)
	}
	expectSmartState(t, e, "manual")
	if e.ResumeSite("synthetic") {
		t.Fatal("retry admitted an overlapping uncooperative probe")
	}
	if !e.ForceResumeSite("synthetic") {
		t.Fatal("force resume rejected")
	}
	flushSmart(t, e)
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	next := expectSmartState(t, e, "cooldown")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), next.NextProbeAt.Add(time.Millisecond))
	flushSmart(t, e)
	// The new incident may enter probing, but its user callback must not launch
	// while the old incident's timed-out callback is still executing.
	var calls atomic.Int32
	e.SetBreakerProbe("synthetic", "", func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		calls.Add(1)
		return completeReadOnlyProbe(ctx, req)
	})
	e.tickBreakers(context.Background(), next.NextProbeAt.Add(time.Millisecond))
	if calls.Load() != 0 {
		t.Fatal("new incident overlapped timed-out extension")
	}
	releaseOnce.Do(func() { close(release) })
	e.tickerWG.Wait()
	if s.probeRunning || s.probeExecuting || s.probeJob != nil {
		t.Fatal("extension exit retained its timeout reservation")
	}
	if s.event.Checks[0].ProbeSuccesses != 0 {
		t.Fatal("timed-out old success mutated new incident")
	}
	completeSmartProbeRound(t, e)
	expectSmartState(t, e, "ramping")
	if calls.Load() != 1 {
		t.Fatalf("new incident could not probe after old extension exited: calls=%d", calls.Load())
	}
}

func TestBreakerProbeRunnerReleasesOwnershipOnCancellationTimeoutAndPanic(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "panic"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := newSmartBreakerFixture(t, 3)
			s := e.breakers.smartBreakers["synthetic"]
			s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := fixtureBreakerProbeJob(s, func(ctx context.Context, _ BreakerProbeRequest) (BreakerProbeResult, error) {
				if kind == "panic" {
					panic("fixture probe panic")
				}
				if kind == "cancel" {
					cancel()
				}
				<-ctx.Done()
				return BreakerProbeResult{}, ctx.Err()
			})
			if kind == "timeout" {
				job.timeout = 5 * time.Millisecond
			}
			e.tickerWG.Add(1)
			e.runBreakerProbe(ctx, job)
			e.tickerWG.Wait()
			if s.probeRunning || s.probeExecuting || s.probeJob != nil || s.probeSettlement != nil {
				t.Fatal("terminated probe left a process-local reservation")
			}
			want := "cooldown"
			if kind == "timeout" {
				want = "manual"
			}
			expectSmartState(t, e, want)
			if s.event.Checks[0].ProbeSuccesses != 0 || !e.ResumeSite("synthetic") {
				t.Fatal("terminated probe certified health or prevented an explicit retry")
			}
		})
	}
}

func TestBreakerProbeStaleOwnerCannotClearNewJobOrSettleItsResult(t *testing.T) {
	for _, replaceState := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_job", true: "new_state"}[replaceState], func(t *testing.T) {
			e, _ := newSmartBreakerFixture(t, 3)
			s := e.breakers.smartBreakers["synthetic"]
			s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
			old := fixtureBreakerProbeJob(s, nil)
			current := old
			if replaceState {
				replacement := *s
				s = &replacement
				e.breakers.smartBreakers["synthetic"] = s
			} else {
				current = fixtureBreakerProbeJob(s, nil)
			}
			result := BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}
			e.breakers.breakerMu.Lock()
			token, reason := e.breakers.settleStageProbeLocked(old, result, nil, context.Background())
			e.breakers.unlockAndEmit()
			e.breakers.probeExited(old)
			if token != nil || reason != "" || !s.probeRunning || !s.probeExecuting || s.probeJob != current || s.event.Checks[0].ProbeSuccesses != 0 {
				t.Fatal("stale callback altered a new job with matching incident and round")
			}
			if replaceState {
				current = fixtureBreakerProbeJob(s, nil)
			}
			// Exercise exit-before-settlement: the owner remains until the result
			// controller has also finished, then milestone fencing takes over.
			e.breakers.probeExited(current)
			if s.probeExecuting || !s.probeRunning || s.probeJob != current {
				t.Fatal("extension exit lost its still-running controller reservation")
			}
			e.breakers.breakerMu.Lock()
			token, reason = e.breakers.settleStageProbeLocked(current, result, nil, context.Background())
			e.breakers.unlockAndEmit()
			if token == nil || reason != "" || s.probeRunning || s.probeJob != nil {
				t.Fatal("current owner could not settle its successful result")
			}
			e.breakers.breakerMu.Lock()
			stale, _ := e.breakers.settleStageProbeLocked(old, result, nil, context.Background())
			e.breakers.unlockAndEmit()
			e.breakers.probeExited(old)
			if stale != nil || s.probeSettlement != token {
				t.Fatal("old callback replaced the current settlement fence")
			}
			e.breakers.breakerMu.Lock()
			e.breakers.finishStageProbeLocked(token, context.Background())
			e.breakers.unlockAndEmit()
			expectSmartState(t, e, "ramping")
		})
	}
}
