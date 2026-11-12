package app

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	adminruntime "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"net/http"
	"sync"
	"time"
)

var runtimeControlScript = adminruntime.Script

type runtimeState struct {
	service *runtimecontrol.Service
	pools   *account.Pools
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (a *App) initRuntime(ctx context.Context) error {
	if err := a.Engine.CheckRuntimeSchema(ctx); err != nil {
		return err
	}
	s := &a.runtimeState
	s.service = runtimecontrol.New(a.Engine, adminruntime.StoreAdapter{Store: a.configStore}, a.Config.Runtime)
	if err := s.service.Initialize(ctx); err != nil {
		return err
	}
	var err error
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if err := a.initBrowserIdentities(); err != nil {
		cancel()
		return err
	}
	if a.accountOnboarding != nil {
		if err := a.accountOnboarding.Load(runCtx); err != nil {
			cancel()
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
				cancel()
				return fmt.Errorf("账号池状态加载失败（请确认 migrate）: %w", err)
			}
		}
	}
	if err := s.service.AttachPools(s.pools); err != nil {
		a.stopRuntime()
		return err
	}
	a.Engine.BindAccountHealthEvents(s.pools)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = a.Engine.Governance().RunSampler(runCtx, governance.NewSampler()) }()
	return nil
}
func (a *App) stopRuntime() {
	_ = a.drainRuntime(5 * time.Second)
}
func (a *App) drainRuntime(timeout time.Duration) bool {
	s := &a.runtimeState
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if a.accountOnboarding != nil {
		if err := a.accountOnboarding.Shutdown(ctx); err != nil {
			return false
		}
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.pools != nil {
		if err := s.pools.Shutdown(ctx); err != nil {
			return false
		}
	}
	return a.stopBrowserIdentities(ctx)
}
func (a *App) runtimeAudit(r *http.Request, table, action, id string, err error) {
	if a.oplog != nil {
		a.oplog.RecordEvent(oplog.Event{Table: table, Action: action, RowID: id, Operator: auth.OperatorFrom(r.Context()), IP: r.RemoteAddr, At: time.Now(), Err: err})
	}
}

func (a *App) runtimeHandler() *adminruntime.Handler {
	return &adminruntime.Handler{Service: a.runtimeState.service, Runtime: a.Engine, Pools: a.runtimeState.pools, Network: a.Config.Network, Audit: a.runtimeAudit}
}
func (a *App) runtimePolicyAPI(w http.ResponseWriter, r *http.Request) {
	a.runtimeHandler().Policy(w, r)
}
func (a *App) runtimeRemoteAPI(w http.ResponseWriter, r *http.Request) {
	a.runtimeHandler().Remote(w, r)
}
