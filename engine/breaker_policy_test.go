package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"sync"
	"testing"
	"time"
)

func TestBreakerPolicyManualTakeoverRetainsFrozenPolicyUntilForceResume(t *testing.T) {
	for _, force := range []bool{false, true} {
		e, _ := newSmartBreakerFixture(t, 1)
		smartFailure(e, 1, "detail", "fault")
		s := e.breakers.smartBreakers["synthetic"]
		old := s.cfg
		next := explicitBreaker(old)
		next.Enabled = false
		next.Policy.MaxRecovery = time.Hour
		if err := e.ApplyBreakerPolicies(2, next, nil); err != nil {
			t.Fatal(err)
		}
		if !e.PauseSite("synthetic", "maintenance") {
			t.Fatal("pause")
		}
		if s.cfg != old {
			t.Fatal("takeover changed recovery policy before ordinary resume")
		}
		if force {
			e.breakers.forceResumeLocked("synthetic")
			if s.cfg.Enabled || s.policyRevision != 2 {
				t.Fatal("force resume did not apply target")
			}
		} else {
			if !e.ResumeSite("synthetic") {
				t.Fatal("resume")
			}
			if s.cfg != s.event.FrozenPolicy || s.policyRevision != s.event.PolicyRevision {
				t.Fatal("resumed incident mixes old identity and new policy")
			}
		}
	}
}

func TestBreakerPolicyTerminalPendingFailureRestoresFrozenPolicy(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "fault")
	s := e.breakers.smartBreakers["synthetic"]
	old := s.cfg
	s.event.PendingFailure = &breaker.PendingFailure{TaskID: 2, Stage: "detail", At: time.Now().Add(-48 * time.Hour), Error: "original fault"}
	e.breakers.transitionBreakerLocked(s, "closed", "fixture", time.Now())
	next := explicitBreaker(old)
	next.Enabled = false
	fresh := &Engine{}
	fresh.RegisterBreakerSite("synthetic")
	if err := fresh.ApplyBreakerPolicies(2, next, nil); err != nil {
		t.Fatal(err)
	}
	pending, _, err := fresh.breakers.restoreSiteLocked(s.snapshot(), time.Now(), map[string]string{"detail": ""})
	if err != nil {
		t.Fatal(err)
	}
	r := fresh.breakers.smartBreakers["synthetic"]
	if pending == nil || r.cfg != old || r.policyRevision != 1 {
		t.Fatal("terminal settlement lost its frozen policy")
	}
	r.savedRevision = r.event.Revision
	state, _, task := fresh.breakers.reserveFailureSettlementLocked("synthetic")
	if state == nil || task == nil {
		t.Fatal("pending failure cannot settle")
	}
	fresh.breakers.recordSmartOutcomeLocked(task, errors.New("retry"), nil, breakerGovernanceEvidence{})
	if r.event.PendingFailure != nil || r.cfg.Enabled || r.policyRevision != 2 {
		t.Fatal("terminal settlement did not release deferred policy")
	}
}

func TestBreakerPolicyReopenedTerminalIncidentRestoresFrozenPolicy(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "fault")
	s := e.breakers.smartBreakers["synthetic"]
	old, id := s.cfg, s.event.ID
	next := explicitBreaker(old)
	next.Enabled = false
	next.Policy.MaxRecovery = time.Hour
	if err := e.ApplyBreakerPolicies(2, next, nil); err != nil {
		t.Fatal(err)
	}
	e.breakers.transitionBreakerLocked(s, "closed", "fixture", time.Now())
	if s.cfg.Enabled {
		t.Fatal("terminal target was not applied")
	}
	now := time.Now()
	e.breakers.beginBreakerRetryLocked(s, now, "manual_retry", "fixture", map[string]string{"detail": ""})
	if s.event.ID != id || s.cfg != old || s.policyRevision != 1 || s.targetRevision != 2 || !s.event.Deadline.Equal(now.Add(old.Policy.MaxRecovery)) {
		t.Fatal("reopened event mixed target policy or recovery budget")
	}
	if !e.BreakerPolicyStatuses()["synthetic"].Deferred {
		t.Fatal("reopened target not deferred")
	}
}

func TestBreakerPolicyRevisionIdentityIncludesUnusedDefaults(t *testing.T) {
	for _, withSite := range []bool{false, true} {
		e := &Engine{}
		c := explicitBreaker(breaker.Config{Window: time.Minute, Threshold: 10})
		var sites map[string]config.BreakerConfig
		if withSite {
			e.RegisterBreakerSite("synthetic")
			sites = map[string]config.BreakerConfig{"synthetic": c}
		}
		if err := e.ApplyBreakerPolicies(1, c, sites); err != nil {
			t.Fatal(err)
		}
		changed := c
		changed.Threshold++
		if err := e.ApplyBreakerPolicies(1, changed, sites); err == nil {
			t.Fatal("same revision changed defaults")
		}
		if err := e.ApplyBreakerPolicies(1, c, sites); err != nil {
			t.Fatal(err)
		}
		if withSite {
			sites["synthetic"] = changed
			if err := e.ApplyBreakerPolicies(1, c, sites); err == nil {
				t.Fatal("publication retained caller's mutable map")
			}
		}
	}
}

func TestBreakerPolicyDirtyTerminalFailureReopensWithFrozenPolicy(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "fault")
	s := e.breakers.smartBreakers["synthetic"]
	old, id := s.cfg, s.event.ID
	next := explicitBreaker(old)
	next.Threshold = 2
	if err := e.ApplyBreakerPolicies(2, next, nil); err != nil {
		t.Fatal(err)
	}
	e.breakers.transitionBreakerLocked(s, "closed", "fixture", time.Now())
	if s.policyRevision != 2 {
		t.Fatal("terminal target not applied")
	}
	// The close has not reached storage. Keep the triggering failure and this
	// durable identity rather than replacing an unsaved report.
	smartFailure(e, 2, "detail", "fault")
	if !s.event.Active() || s.event.ID != id || s.cfg != old || s.policyRevision != s.event.PolicyRevision || s.targetRevision != 2 {
		t.Fatal("dirty terminal reopen lost its trigger or frozen policy")
	}
	if e.breakerSum("synthetic") != 2 {
		t.Fatal("reopen lost evidence")
	}
}

func explicitBreaker(c breaker.Config) config.BreakerConfig {
	c = breaker.EffectiveConfig(c)
	return config.BreakerConfig{Enabled: c.Enabled, Window: c.Window, Threshold: c.Threshold, Policy: c.Policy}
}
func TestBreakerPolicyActiveStagesFreezeAndRestore(t *testing.T) {
	for _, state := range []string{"observing", "cooldown", "probing", "ramping", "manual"} {
		t.Run(state, func(t *testing.T) {
			e, _ := newSmartBreakerFixture(t, 1)
			smartFailure(e, 1, "detail", "broken")
			s := e.breakers.smartBreakers["synthetic"]
			s.event.State = state
			old := s.cfg
			deadline := s.event.Deadline
			id := s.event.ID
			next := explicitBreaker(old)
			next.Enabled = false
			next.Window = time.Second
			next.Policy.Capacity = 8
			next.Policy.MaxRecovery = time.Hour
			if err := e.ApplyBreakerPolicies(2, next, nil); err != nil {
				t.Fatal(err)
			}
			if s.cfg != old || s.policy != old.Policy || s.event.Deadline != deadline || s.event.ID != id {
				t.Fatal("active policy or budget changed")
			}
			if got := e.BreakerPolicyStatuses()["synthetic"]; !got.Deferred || got.IncidentRevision != 1 || got.TargetRevision != 2 {
				t.Fatalf("status=%+v", got)
			}
			snap := s.snapshot()
			if err := snap.ValidateCurrent(); err != nil {
				t.Fatal(err)
			}
			fresh := &Engine{}
			fresh.RegisterBreakerSite("synthetic")
			if err := fresh.ApplyBreakerPolicies(2, next, nil); err != nil {
				t.Fatal(err)
			}
			_, _, err := fresh.breakers.restoreSiteLocked(snap, time.Now(), map[string]string{"detail": ""})
			if err != nil {
				t.Fatal(err)
			}
			restored := fresh.breakers.smartBreakers["synthetic"]
			if restored.cfg != old || restored.event.Deadline != deadline || restored.event.PolicyRevision != 1 {
				t.Fatal("restart replaced frozen policy/budget")
			}
			e.breakers.beginBreakerRetryLocked(s, time.Now(), "manual_retry", "fixture", map[string]string{"detail": ""})
			if s.cfg != old || s.event.FrozenPolicy != old || s.event.PolicyRevision != 1 {
				t.Fatal("retry refreshed frozen policy")
			}
			e.breakers.transitionBreakerLocked(s, "closed", "fixture", time.Now())
			s.applyTargetPolicy(time.Now())
			if s.cfg.Enabled || s.policyRevision != 2 || e.breakerSum("synthetic") != 1 {
				t.Fatal("terminal policy switch lost evidence")
			}
		})
	}
}
func TestBreakerPolicyUpdateDoesNotReleaseManualPause(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	if !e.PauseSite("synthetic", "maintenance") {
		t.Fatal("pause")
	}
	c := explicitBreaker(e.breakers.smartBreakers["synthetic"].cfg)
	c.Threshold = 1
	if err := e.ApplyBreakerPolicies(2, c, nil); err != nil {
		t.Fatal(err)
	}
	if !e.BreakerStatuses()["synthetic"].Paused {
		t.Fatal("policy released pause")
	}
	if !e.ResumeSite("synthetic") {
		t.Fatal("resume")
	}
	if e.breakers.smartBreakers["synthetic"].cfg.Threshold != 1 {
		t.Fatal("resume lost target")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	cancel()
	if err := e.ApplyBreakerPolicies(3, c, nil); err == nil {
		t.Fatal("shutdown accepted policy")
	}
}
func TestBreakerPolicyPublicationValidationIsAtomic(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 10)
	c := explicitBreaker(e.breakers.smartBreakers["synthetic"].cfg)
	if err := e.ApplyBreakerPolicies(2, c, map[string]config.BreakerConfig{"missing": c}); err == nil {
		t.Fatal("unknown site")
	}
	if e.breakers.smartBreakers["synthetic"].targetRevision != 1 {
		t.Fatal("partial publication")
	}
	if err := e.ApplyBreakerPolicies(2, c, nil); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Threshold++
	if err := e.ApplyBreakerPolicies(2, changed, nil); err == nil {
		t.Fatal("same revision changed content")
	}
	if err := e.ApplyBreakerPolicies(1, c, nil); err == nil {
		t.Fatal("stale version")
	}
}

func TestBreakerPolicyConcurrentPublicationPreservesIncident(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "fault")
	initial := e.breakers.smartBreakers["synthetic"].event.ID
	c := explicitBreaker(e.breakers.smartBreakers["synthetic"].cfg)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for rev := uint64(2); rev <= 100; rev++ {
			next := c
			next.Enabled = rev%2 == 0
			next.Threshold = int(rev)
			if err := e.ApplyBreakerPolicies(rev, next, nil); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for id := 2; id <= 100; id++ {
			smartFailure(e, id, "detail", "fault")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			e.BreakerPolicyStatuses()
			e.BreakerStatuses()
		}
	}()
	wg.Wait()
	s := e.breakers.smartBreakers["synthetic"]
	if s.event.ID != initial || s.event.PolicyRevision != 1 || s.targetRevision != 100 || s.cfg.Threshold != 1 {
		t.Fatal("concurrent updates replaced incident")
	}
}
func TestBreakerPolicyMustLoadBeforeStartup(t *testing.T) {
	e := &Engine{}
	e.RegisterBreakerSite("fixture")
	if err := e.InitBreakerRuntime(context.Background()); err == nil {
		t.Fatal("startup accepted missing database policy")
	}
	if _, ok := e.breakers.admitBreakerTask(&Task{Site: "fixture"}); ok {
		t.Fatal("task admitted before database policy")
	}
}
