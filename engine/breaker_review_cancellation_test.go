package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

type breakerCancellationEmitter struct {
	action string
	cancel context.CancelFunc
}

func (e breakerCancellationEmitter) Emit(ev events.Event) {
	if ev.Attrs["action"] == e.action {
		e.cancel()
	}
}

func TestBreakerProbeCancellationDuringResultEventCannotEnableRamping(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.SetEventsEmitter(breakerCancellationEmitter{action: "stage_probe_passed", cancel: cancel})
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	s.probeRunning, s.probeExecuting = true, true
	e.tickerWG.Add(1)
	e.runBreakerProbe(ctx, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil
	}, BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, time.Minute)
	e.tickerWG.Wait()
	if ctx.Err() == nil {
		t.Fatal("fixture did not cancel while publishing probe result")
	}
	if s.event.State == "ramping" {
		t.Fatal("cancelled recovery enabled ramping using stale parent cancellation snapshot")
	}
}

func TestBreakerProbeCancellationDuringFailureEventUsesShutdownTransition(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.SetEventsEmitter(breakerCancellationEmitter{action: "stage_probe_failed", cancel: cancel})
	s := e.breakers.smartBreakers["synthetic"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "synthetic", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	s.probeRunning, s.probeExecuting = true, true
	e.tickerWG.Add(1)
	e.runBreakerProbe(ctx, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{}, context.DeadlineExceeded
	}, BreakerProbeRequest{Site: "synthetic", Stage: "detail"}, "incident", 1, time.Minute)
	e.tickerWG.Wait()
	if ctx.Err() == nil {
		t.Fatal("fixture did not cancel while publishing failed probe")
	}
	if s.event.State != "cooldown" {
		t.Fatalf("cancelled recovery state=%s; want shutdown cooldown rather than timeout manual", s.event.State)
	}
}
