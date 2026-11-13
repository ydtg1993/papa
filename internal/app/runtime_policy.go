package app

import (
	"context"
	"fmt"
	adminruntime "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"time"
)

type runtimeState struct {
	service *runtimecontrol.Service
	pools   *account.Pools
	cancel  context.CancelFunc
	done    chan struct{}
}

func (a *App) initRuntime(ctx context.Context) error {
	s := &a.runtimeState
	// A business-supplied manager may already own its coordinator. Adopt that
	// lifetime before validation can fail so startup cleanup still stops it.
	if m := a.Engine.GetAccountManager(); m != nil {
		s.pools = m.Pools()
	}
	if err := a.Engine.CheckRuntimeSchema(ctx); err != nil {
		return err
	}
	s.service = runtimecontrol.New(a.Engine, adminruntime.StoreAdapter{Store: a.configStore}, a.Config.Runtime)
	if err := s.service.Initialize(ctx); err != nil {
		return err
	}
	var err error
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if err := a.initBrowserIdentities(); err != nil {
		return err
	}
	if a.accountOnboarding != nil {
		if err := a.accountOnboarding.Load(runCtx); err != nil {
			return fmt.Errorf("账号资料加载失败（请确认 migrate 与站点适配器）: %w", err)
		}
	}
	if m := a.Engine.GetAccountManager(); m != nil {
		if existing := m.Pools(); existing != nil {
			s.pools = existing
		} else {
			s.pools, err = account.NewPools(runCtx, m, account.NewGORMPoolStore(a.DB), account.PoolOptions{References: a.Engine.AccountPoolReferences, Context: func(ctx context.Context, key account.Key) context.Context {
				return a.Engine.RuntimeContext(ctx, key.Site)
			}})
			if err != nil {
				return fmt.Errorf("账号池状态加载失败（请确认 migrate）: %w", err)
			}
		}
	}
	if err := s.service.AttachPools(s.pools); err != nil {
		return err
	}
	a.Engine.BindAccountHealthEvents(s.pools)
	a.startRuntimeSampler(runCtx, governance.NewSampler())
	return nil
}

func (a *App) startRuntimeSampler(ctx context.Context, sampler governance.Sampler) {
	done := make(chan struct{})
	a.runtimeState.done = done
	go func() {
		defer close(done)
		_ = a.Engine.Governance().RunSampler(ctx, sampler)
	}()
}

func (a *App) stopRuntime() {
	_ = a.drainRuntime(5 * time.Second)
}
func (a *App) drainRuntime(timeout time.Duration) bool {
	s := &a.runtimeState
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Stop every producer/admission gate before waiting for any component.
	// A timeout never leaves a later component accepting new work.
	if a.accountOnboarding != nil {
		a.accountOnboarding.BeginShutdown()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.pools != nil {
		s.pools.BeginShutdown()
	}
	a.identityState.BeginShutdown()

	drained := true
	if a.accountOnboarding != nil && a.accountOnboarding.Shutdown(ctx) != nil {
		drained = false
	}
	if !waitAppLoop(ctx, s.done) {
		drained = false
	}
	if s.pools != nil && s.pools.Shutdown(ctx) != nil {
		drained = false
	}
	if !a.stopBrowserIdentities(ctx) {
		drained = false
	}
	return drained
}
