package app

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"sort"
	"time"
)

// The adapter sees only policy publication and detached diagnostics, not the
// engine lifecycle or resource construction surface.
type breakerPolicyTarget interface {
	ApplyBreakerPolicies(uint64, config.BreakerConfig, map[string]config.BreakerConfig) error
	BreakerPolicyStatuses() map[string]engine.BreakerPolicyStatus
}
type browserPolicyTarget interface {
	ApplyLimits(int, int, time.Duration) error
	Stats() browser.PoolStats
}
type executionTarget struct {
	breakers       breakerPolicyTarget
	browser        browserPolicyTarget
	browserEnabled bool
}

func (t executionTarget) Apply(ctx context.Context, revision uint64, d ep.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.browserEnabled && t.browser == nil {
		return fmt.Errorf("enabled browser pool was not initialized")
	}
	sites := make(map[string]config.BreakerConfig, len(d.Breaker.Sites))
	for k, b := range d.Breaker.Sites {
		sites[k] = b.Native()
	}
	if err := t.breakers.ApplyBreakerPolicies(revision, d.Breaker.Defaults.Native(), sites); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.browserEnabled {
		return t.browser.ApplyLimits(d.Browser.PoolSize, d.Browser.DirectPoolSize, time.Duration(d.Browser.MaxIdleTime))
	}
	return nil
}
func (t executionTarget) Status() any {
	v := map[string]any{"browser_enabled": t.browserEnabled, "breakers": t.breakers.BreakerPolicyStatuses()}
	if t.browser != nil {
		v["browser"] = t.browser.Stats()
	}
	return v
}
func (a *App) executionSites() []string {
	var sites []string
	for k := range a.Engine.BreakerPolicyStatuses() {
		sites = append(sites, k)
	}
	sort.Strings(sites)
	return sites
}
func (a *App) initExecutionPolicy(ctx context.Context) error {
	store := configstore.ExecutionStore{Store: a.configStore}
	sites := a.executionSites()
	// Browser construction/self-check belongs to startup, never to live Apply.
	// Validate persisted inputs before starting any browser process.
	h, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load execution_policy (run explicit policy initialization): %w", err)
	}
	if h.Revision == 0 {
		return fmt.Errorf("execution policy revision must be positive")
	}
	if err = h.Document.Validate(sites); err != nil {
		return err
	}
	if err = a.Engine.InitBrowserPool(ctx, h.Document.Browser); err != nil {
		return err
	}
	target := executionTarget{breakers: a.Engine, browserEnabled: a.Config.Browser.Enable}
	if pool := a.Engine.GetBrowserPool(); pool != nil {
		target.browser = pool
	}
	a.executionPolicy = ep.New(store, target, sites)
	return a.executionPolicy.Initialize(ctx)
}

// ExecutionPolicy returns the same versioned service used by OA, after Run initialization.
func (a *App) ExecutionPolicy() ep.Service { return a.executionPolicy }

// InitializeExecutionPolicy explicitly creates revision one; it never overwrites an existing policy.
// RegisterSites first, and migrate explicitly before calling this method.
func (a *App) InitializeExecutionPolicy(ctx context.Context, d ep.Document, operator string) error {
	if err := d.Validate(a.executionSites()); err != nil {
		return err
	}
	if operator == "" {
		return fmt.Errorf("initialization requires operator")
	}
	_, err := (configstore.ExecutionStore{Store: a.configStore}).Save(ctx, ep.Update{ExpectedRevision: 0, OperationID: "bootstrap-" + fmt.Sprint(time.Now().UnixNano()), Document: d, Reason: "explicit initialization"}, operator)
	return err
}
