package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"time"
)

func (e *Engine) Governance() *governance.Controller {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.governor
}

// ResolveRuntime constructs a fresh policy, preserving precedence and rejecting
// references to undeclared sites/stages before any database write.
func (e *Engine) ResolveRuntime(override config.RuntimeConfig) (governance.Config, map[string]time.Duration, error) {
	base := governance.DefaultConfig()
	file := e.cfg.Runtime
	base.Defaults = governance.MergePolicy(governance.MergePolicy(base.Defaults, file.Defaults), override.Defaults)
	base.Resources = governance.MergeResources(governance.MergeResources(base.Resources, file.Resources), override.Resources)
	base.Sites = map[string]governance.PolicyPatch{}
	sites := map[string]governance.Policy{}
	delays := map[string]time.Duration{}
	for _, info := range e.stages {
		p, ok := sites[info.config.Site]
		if !ok {
			p = governance.DefaultConfig().Defaults
			p.Stages = map[string]governance.StagePolicy{}
		}
		p.Stages[info.stage] = governance.StagePolicy{MaxConcurrent: info.config.WorkerCount}
		sites[info.config.Site] = p
	}
	for site := range file.Sites {
		if _, ok := sites[site]; !ok {
			return base, nil, fmt.Errorf("runtime: 未注册站点 %q", site)
		}
	}
	for site := range override.Sites {
		if _, ok := sites[site]; !ok {
			return base, nil, fmt.Errorf("runtime: 未注册站点 %q", site)
		}
	}
	for site, p := range sites {
		explicitStagePool := map[string]bool{}
		for _, patch := range []governance.PolicyPatch{file.Defaults, file.Sites[site], override.Defaults, override.Sites[site]} {
			for stage, s := range patch.Stages {
				info := e.stages[core.StageKey(site, stage)]
				if info == nil {
					return base, nil, fmt.Errorf("runtime: 站点 %q 未注册阶段 %q", site, stage)
				}
				if s.MaxConcurrent != nil && *s.MaxConcurrent > info.config.WorkerCount {
					return base, nil, fmt.Errorf("runtime: %s:%s 并发超过物理 worker 上限 %d", site, stage, info.config.WorkerCount)
				}
				if s.Delay != nil {
					delays[core.StageKey(site, stage)] = *s.Delay
				}
				if s.Pool != nil {
					explicitStagePool[stage] = true
				}
			}
			p = governance.MergePolicy(p, patch)
		}
		for stage, policy := range p.Stages {
			if !explicitStagePool[stage] {
				policy.Pool = p.Pool
				p.Stages[stage] = policy
			}
		}
		base.Sites[site] = governance.PatchOfPolicy(p)
	}
	return base, delays, base.Validate()
}

func (e *Engine) InstallGovernance(c *governance.Controller, delays map[string]time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.governor = c
	e.runtimeDelays = delays
}
func (e *Engine) taskDelay(task *Task, fallback time.Duration) time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if d, ok := e.runtimeDelays[stageKeyOf(task)]; ok {
		return d
	}
	return fallback
}

// RuntimeDeclarations exposes physical capacity and the original delay range;
// a random declaration must not be misrepresented as a zero-second override.
func (e *Engine) RuntimeDeclarations() map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, info := range e.stages {
		if out[info.config.Site] == nil {
			out[info.config.Site] = map[string]any{}
		}
		out[info.config.Site][info.stage] = map[string]any{"physical_workers": info.config.WorkerCount, "queue_capacity": info.config.QueueSize, "delay_min": info.config.Delay.Min, "delay_max": info.config.Delay.Max}
	}
	return out
}

func (e *Engine) CheckRuntimeSchema(ctx context.Context) error {
	var rows []models.CrawlerTask
	if err := e.db.WithContext(ctx).Select("account_pool").Limit(0).Find(&rows).Error; err != nil {
		return fmt.Errorf("任务账号池列不可用，请执行 migrate: %w", err)
	}
	return nil
}

// RuntimeContext also supports explicitly triggered account verification and
// business calls outside a task. Inside task handlers it is supplied automatically.
func (e *Engine) RuntimeContext(ctx context.Context, site string) context.Context {
	return governance.WithSite(governance.WithController(ctx, e.Governance()), site)
}

func (e *Engine) resolveTaskPool(task *Task) string {
	if task.AccountPool != "" {
		return task.AccountPool
	}
	c := e.Governance()
	if c == nil {
		return ""
	}
	p := c.Effective(task.Site)
	if s, ok := p.Stages[task.Stage]; ok {
		return s.Pool
	}
	return p.Pool
}

func (e *Engine) prepareRuntimeTask(ctx context.Context, task *Task, physical int) (context.Context, func(), error) {
	baseCtx := e.RuntimeContext(ctx, task.Site)
	for {
		ctx = baseCtx
		// Accounts and remote cooldown are checked before obtaining global execution
		// slots. No DB transaction or engine lock is held while waiting.
		if name := task.AccountPool; name != "" {
			m := e.GetAccountManager()
			if m == nil || m.Pools() == nil {
				return ctx, nil, fmt.Errorf("账号池 %s:%s 未配置", task.Site, name)
			}
			a, err := m.Pools().Acquire(ctx, account.PoolKey{Site: task.Site, Name: name}, "")
			if err != nil {
				return ctx, nil, err
			}
			a.TaskID = uint64(task.ID)
			ctx = account.WithAttempt(ctx, a)
		}
		c := e.Governance()
		if c == nil {
			return ctx, func() {}, nil
		}
		if err := c.WaitTaskReady(ctx, task.Site); err != nil {
			return ctx, nil, err
		}
		release, err := c.AcquireStage(ctx, task.Site, task.Stage, physical)
		if err != nil {
			return ctx, nil, err
		}
		if identity, ok := account.AttemptFromContext(ctx); ok {
			m := e.GetAccountManager()
			if m == nil || m.Pools() == nil || m.Pools().ValidateAttempt(identity) != nil {
				release()
				continue
			}
		}
		return ctx, release, nil
	}
}
