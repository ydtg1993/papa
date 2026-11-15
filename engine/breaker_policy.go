package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"maps"
	"time"
)

// RegisterBreakerSite registers identity only. ApplyBreakerPolicies supplies the
// database policy before InitBreakerRuntime and before any tasks are admitted.
func (e *Engine) RegisterBreakerSite(site string) { e.breakers.registerBreakerSite(site) }

func (b *breakerCoordinator) registerBreakerSite(site string) {
	if site == "" {
		return
	}
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if b.smartBreakers == nil {
		b.smartBreakers = make(map[string]*smartBreaker)
	}
	if b.smartBreakers[site] == nil {
		c := breaker.EffectiveConfig(breaker.Config{})
		b.smartBreakers[site] = &smartBreaker{cfg: c, policy: c.Policy, detector: breaker.NewDetector(c)}
	}
}

// ApplyBreakerPolicies atomically publishes fully expanded policies. Active
// incidents retain their frozen detector and recovery policy until termination.
func (e *Engine) ApplyBreakerPolicies(revision uint64, defaults config.BreakerConfig, sites map[string]config.BreakerConfig) error {
	e.mu.RLock()
	ctx := e.ctx
	e.mu.RUnlock()
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("breaker policy target is stopping")
	}
	return e.breakers.applyBreakerPolicies(ctx, revision, defaults, sites)
}
func (b *breakerCoordinator) applyBreakerPolicies(ctx context.Context, revision uint64, defaults config.BreakerConfig, sites map[string]config.BreakerConfig) error {
	if revision == 0 {
		return fmt.Errorf("breaker policy revision must be positive")
	}
	convert := func(c config.BreakerConfig) breaker.Config {
		return breaker.Config{Enabled: c.Enabled, Window: c.Window, Threshold: c.Threshold, Policy: c.Policy}
	}
	base := convert(defaults)
	if err := breaker.ValidateExplicit(base); err != nil {
		return err
	}
	overrides := make(map[string]breaker.Config, len(sites))
	for site, c := range sites {
		v := convert(c)
		if err := breaker.ValidateExplicit(v); err != nil {
			return fmt.Errorf("site %s: %w", site, err)
		}
		overrides[site] = v
	}
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("breaker policy target is stopping")
	}
	if revision < b.policyRevision {
		return fmt.Errorf("stale breaker policy revision %d", revision)
	}
	if revision == b.policyRevision && (b.policyDefaults != base || !maps.Equal(b.policyOverrides, overrides)) {
		return fmt.Errorf("breaker revision reused with different content")
	}
	for site := range overrides {
		if b.smartBreakers[site] == nil {
			return fmt.Errorf("unknown breaker site %q", site)
		}
	}
	for site, s := range b.smartBreakers {
		c := base
		if override, ok := overrides[site]; ok {
			c = override
		}
		if revision == b.policyRevision && (s.targetRevision != revision || s.target != c) {
			return fmt.Errorf("breaker revision reused with different content")
		}
	}
	now := time.Now()
	for site, s := range b.smartBreakers {
		c := base
		if override, ok := overrides[site]; ok {
			c = override
		}
		s.target, s.targetRevision = c, revision
		s.applyTargetPolicy(now)
	}
	b.policyRevision = revision
	b.policyDefaults, b.policyOverrides = base, overrides
	return nil
}

func (s *smartBreaker) applyTargetPolicy(now time.Time) {
	if s.targetRevision == 0 || s.policyRevision == s.targetRevision || s.manualTakeover || (s.event != nil && (s.event.Active() || s.event.PendingFailure != nil)) {
		return
	}
	// Already validated before publication; detector mutation cannot fail.
	_ = s.detector.UpdateConfig(s.target, now)
	s.cfg, s.policy, s.policyRevision = s.target, s.target.Policy, s.targetRevision
}

// BreakerPolicyStatus is detached diagnostic state, safe for API serialization.
type BreakerPolicyStatus struct {
	TargetRevision    uint64 `json:"target_revision"`
	EffectiveRevision uint64 `json:"effective_revision"`
	IncidentRevision  uint64 `json:"incident_policy_revision,omitempty"`
	IncidentID        string `json:"incident_id,omitempty"`
	Deferred          bool   `json:"deferred"`
}

func (e *Engine) BreakerPolicyStatuses() map[string]BreakerPolicyStatus {
	return e.breakers.policyStatuses()
}
func (b *breakerCoordinator) policyStatuses() map[string]BreakerPolicyStatus {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	out := make(map[string]BreakerPolicyStatus, len(b.smartBreakers))
	for site, s := range b.smartBreakers {
		st := BreakerPolicyStatus{TargetRevision: s.targetRevision, EffectiveRevision: s.policyRevision}
		if s.event != nil && (s.event.Active() || s.event.PendingFailure != nil || s.manualTakeover) {
			st.IncidentRevision = s.event.PolicyRevision
			st.IncidentID = s.event.ID
			st.Deferred = s.targetRevision != s.event.PolicyRevision
		}
		out[site] = st
	}
	return out
}

func (b *breakerCoordinator) policiesReady() bool {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if b.policyRevision == 0 {
		return false
	}
	for _, s := range b.smartBreakers {
		if s.targetRevision == 0 {
			return false
		}
	}
	return true
}
