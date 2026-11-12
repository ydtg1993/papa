package app

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/diagnostics"
	"github.com/ydtg1993/papa/v4/internal/browserdiagnostics"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"net/http"
)

type browserIdentityState = browserdiagnostics.Service

func (a *App) diagnosticService() *browserdiagnostics.Service {
	options := browserdiagnostics.Options{}
	if a.Config != nil {
		options.Config = &a.Config.Browser
	}
	if a.Engine != nil {
		options.Resources = a.Engine
		options.HasSite = func(site string) bool { _, ok := a.Engine.Site(site); return ok }
	}
	if a.Logger != nil && a.Logger.Sys != nil {
		options.Log = a.Logger.Sys.Infof
	}
	a.identityState.Configure(options)
	return &a.identityState
}

// BrowserIdentities returns the identity service without launching a browser.
func (a *App) BrowserIdentities() *identityruntime.Manager { return a.identityState.Manager() }
func (a *App) initBrowserIdentities() error {
	s := a.diagnosticService()
	if err := s.Initialize(identityruntime.NewGORMStore(a.DB)); err != nil {
		return err
	}
	m := s.Manager()
	a.Engine.SetIdentityManager(m)
	if accounts := a.Engine.GetAccountManager(); accounts != nil {
		accounts.SetIdentityManager(m)
	}
	return nil
}
func (a *App) diagnosticHandler() *diagnostics.Handler {
	return &diagnostics.Handler{Service: a.diagnosticService(),
		Sites: func() any { return a.Engine.GetSiteStats() },
		Audit: a.runtimeAudit,
	}
}
func (a *App) browserIdentitiesAPI(w http.ResponseWriter, r *http.Request) {
	a.diagnosticHandler().Identities(w, r)
}

func (a *App) stopBrowserIdentities(ctx context.Context) bool { return a.identityState.Shutdown(ctx) }

func (a *App) identityView(ctx context.Context, site string) (map[string]any, error) {
	view, err := a.identityState.View(ctx, site)
	if view != nil {
		view["sites"] = a.Engine.GetSiteStats()
	}
	return view, err
}
