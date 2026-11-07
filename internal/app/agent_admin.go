package app

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/admin/server"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

// UseAgentCenter registers host-approved analysis profiles before Run. Reading
// audit runs only needs agent.enabled; execution also needs rules and quality.
// App owns service shutdown, but the host retains ownership of model/blob deps.
func (a *App) UseAgentCenter(options agentadmin.Options) error {
	if a.cancel != nil || a.agentCenter != nil || a.Config == nil || !a.Config.Agent.Enabled {
		return agentadmin.ErrInvalid
	}
	service, err := a.newAgentCenter(options)
	if err != nil {
		return err
	}
	a.agentCenter = service
	return nil
}
func (a *App) newAgentCenter(options agentadmin.Options) (*agentadmin.Service, error) {
	audit, err := agentstore.New(a.DB)
	if err != nil {
		return nil, err
	}
	cfg := agentadmin.Config{Audit: audit, Profiles: options.Profiles, MaxConcurrent: options.MaxConcurrent, WorkerStopped: options.WorkerStopped}
	if a.Config.Quality.Enabled {
		cfg.Quality, err = rulequality.New(a.DB)
		if err != nil {
			return nil, err
		}
	}
	if a.Config.Rules.Enabled {
		cfg.Rules, err = rulestore.NewAdmin(a.DB)
		if err != nil {
			return nil, err
		}
	}
	return agentadmin.New(cfg)
}
func (a *App) agentAdministration() server.AgentAdministration {
	if !a.Config.Agent.Enabled {
		return nil
	}
	if a.agentCenter == nil {
		service, err := a.newAgentCenter(agentadmin.Options{})
		if err != nil {
			panic(fmt.Errorf("Agent 中心初始化失败: %w", err))
		}
		a.agentCenter = service
	}
	return a.agentCenter
}
func (a *App) stopAgentCenter(timeout time.Duration) bool {
	if a.agentCenter == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := a.CloseAgentCenter(ctx); err != nil {
		a.Logger.Sys.Errorf("Agent 后台任务未排空，保留数据库与依赖直到进程退出")
		return false
	}
	return true
}

// CloseAgentCenter cancels this App's analyses and waits for their audit cleanup.
// A host can call it again after Run returns before closing its own blob/model
// dependencies if shutdown previously timed out. It never resumes a run.
func (a *App) CloseAgentCenter(ctx context.Context) error {
	if ctx == nil {
		return agentadmin.ErrInvalid
	}
	if a.agentCenter == nil {
		return nil
	}
	return a.agentCenter.Close(ctx)
}
