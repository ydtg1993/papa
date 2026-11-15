package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
)

type fakeBreakerPolicyTarget struct {
	revision   uint64
	err        error
	afterApply func()
}

func (t *fakeBreakerPolicyTarget) ApplyBreakerPolicies(revision uint64, _ config.BreakerConfig, _ map[string]config.BreakerConfig) error {
	if t.err != nil {
		return t.err
	}
	t.revision = revision
	if t.afterApply != nil {
		t.afterApply()
	}
	return nil
}
func (t *fakeBreakerPolicyTarget) BreakerPolicyStatuses() map[string]engine.BreakerPolicyStatus {
	return map[string]engine.BreakerPolicyStatus{"fixture": {TargetRevision: t.revision}}
}

type fakeBrowserPolicyTarget struct {
	calls int
	err   error
}

func (t *fakeBrowserPolicyTarget) ApplyLimits(int, int, time.Duration) error { t.calls++; return t.err }
func (t *fakeBrowserPolicyTarget) Stats() browser.PoolStats                  { return browser.PoolStats{} }
func TestExecutionTargetRequiresInitializedEnabledPool(t *testing.T) {
	breaker := &fakeBreakerPolicyTarget{}
	target := executionTarget{breakers: breaker, browserEnabled: true}
	if err := target.Apply(context.Background(), 1, ep.InitialDocument()); err == nil {
		t.Fatal("enabled missing resource accepted")
	}
	if breaker.revision != 0 {
		t.Fatal("missing pool partially applied breaker policy")
	}
	target.browserEnabled = false
	if err := target.Apply(context.Background(), 1, ep.InitialDocument()); err != nil {
		t.Fatal(err)
	}
}
func TestExecutionTargetReportsPartialFailureAndCanRetry(t *testing.T) {
	breaker := &fakeBreakerPolicyTarget{}
	pool := &fakeBrowserPolicyTarget{err: errors.New("pool closed")}
	target := executionTarget{breakers: breaker, browser: pool, browserEnabled: true}
	if err := target.Apply(context.Background(), 2, ep.InitialDocument()); err == nil || breaker.revision != 2 || pool.calls != 1 {
		t.Fatal("partial component failure hidden", err)
	}
	pool.err = nil
	if err := target.Apply(context.Background(), 2, ep.InitialDocument()); err != nil || pool.calls != 2 {
		t.Fatal("same generation retry failed", err)
	}
}
func TestExecutionTargetCancellationBetweenComponents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	breaker := &fakeBreakerPolicyTarget{afterApply: cancel}
	pool := &fakeBrowserPolicyTarget{}
	target := executionTarget{breakers: breaker, browser: pool, browserEnabled: true}
	if err := target.Apply(ctx, 1, ep.InitialDocument()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if pool.calls != 0 {
		t.Fatal("browser policy applied after target context cancellation")
	}
}
