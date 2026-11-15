package engine

import "github.com/ydtg1993/papa/v4/internal/breaker"

// SetSiteBreaker registers the site's single outcome detector before Run.
func (e *Engine) SetSiteBreaker(site string, cfg breaker.Config) {
	e.breakers.SetSiteBreaker(site, cfg)
}

func (b *breakerCoordinator) SetSiteBreaker(site string, cfg breaker.Config) {
	if site == "" {
		return
	}
	cfg = breaker.EffectiveConfig(cfg)
	detector := breaker.NewDetector(cfg)
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if b.smartBreakers == nil {
		b.smartBreakers = make(map[string]*smartBreaker)
	}
	b.policyRevision = 1
	b.smartBreakers[site] = &smartBreaker{cfg: cfg, target: cfg, targetRevision: 1, policyRevision: 1, policy: cfg.Policy, detector: detector}
}
