package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
)

// Cancellation belongs to the recovery protocol, not to observer delivery.
// Exercise the exact boundary deterministically without a callback side effect.
func TestBreakerProbeCancellationAfterMilestoneCannotEnableRamping(t *testing.T) {
	testBreakerCancellationAfterMilestone(t, true)
}

func TestBreakerProbeCancellationAfterFailureUsesShutdownTransition(t *testing.T) {
	testBreakerCancellationAfterMilestone(t, false)
}

func testBreakerCancellationAfterMilestone(t *testing.T, healthy bool) {
	t.Helper()
	e, _ := newSmartBreakerFixture(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	result := BreakerProbeResult{Healthy: healthy, Semantic: healthy, Detail: "healthy"}
	var resultErr error
	if !healthy {
		resultErr = context.DeadlineExceeded
	}
	e.breakers.breakerMu.Lock()
	token, reason := e.breakers.settleStageProbeLocked(BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, result, resultErr, ctx)
	e.breakers.unlockAndEmit()
	if token == nil || reason != "" {
		t.Fatalf("missing settlement: %v %s", token, reason)
	}
	cancel()
	e.breakers.breakerMu.Lock()
	e.breakers.finishStageProbeLocked(token, ctx)
	e.breakers.unlockAndEmit()
	if s.probeSettlement != nil {
		t.Fatal("settlement token retained")
	}
	if healthy && s.event.State == "ramping" {
		t.Fatal("cancelled recovery enabled ramping")
	}
	if !healthy && s.event.State != "cooldown" {
		t.Fatalf("cancelled recovery state=%s; want shutdown cooldown", s.event.State)
	}
}
