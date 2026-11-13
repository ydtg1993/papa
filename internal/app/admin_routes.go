package app

import (
	"net/http"

	"github.com/ydtg1993/papa/v4/admin/accountpools"
	"github.com/ydtg1993/papa/v4/admin/accounts"
	"github.com/ydtg1993/papa/v4/admin/diagnostics"
	"github.com/ydtg1993/papa/v4/admin/page"
	adminruntime "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/admin/server"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
)

// registerBuiltinPages selects features and preserves the historical resource
// order. Each feature owns its default page metadata and resource contract.
func (a *App) registerBuiltinPages() {
	pages := []page.Descriptor{diagnostics.Page(), adminruntime.Page(), accountpools.Page(), accounts.Page()}
	if a.Config.TaskFlow.Enabled {
		p := taskflow.Page()
		// These public facade variables have historically allowed hosts to
		// override TaskFlow resources before Run. Keep that compatibility.
		p.Script, p.CSS = server.TaskFlowScript, server.TaskFlowCSS
		pages = append(pages, p)
	}
	for _, p := range pages {
		a.UseCSS(p.CSS)
		a.UsePage(Page{Key: p.Key, Label: p.Label, Group: p.Group, Section: p.Section, Script: p.Script})
	}
}

// mountAdminRoutes constructs one adapter per feature. Private per-request
// forwarding methods are unnecessary; the public extension factories remain.
func (a *App) mountAdminRoutes(mux *http.ServeMux, guard Middleware) {
	diagnostic := a.diagnosticHandler()
	runtime := a.runtimeHandler()
	pools := a.accountPoolsHandler()
	accounts := a.accountsHandler()
	for _, route := range []struct {
		path string
		http.HandlerFunc
	}{
		{"/api/browser-diagnostics", diagnostic.Rod},
		{"/api/browser-identities", diagnostic.Identities},
		{"/api/runtime-policy", runtime.Policy},
		{"/api/runtime-remote", runtime.Remote},
		{"/api/account-pools", pools.PoolsAPI},
		{"/api/accounts", accounts.Accounts},
		{"/api/accounts/remote", accounts.Remote},
		{"/api/account-compensation", pools.Compensation},
		{"/api/runtime-policy/history", runtime.History},
		{"/api/proxy-health", runtime.ProxyHealth},
	} {
		mux.Handle(route.path, guard(route.HandlerFunc))
	}
}

func (a *App) diagnosticHandler() *diagnostics.Handler {
	return &diagnostics.Handler{Service: a.diagnosticService(), Sites: func() any { return a.Engine.GetSiteStats() }, Audit: a.runtimeAudit}
}

func (a *App) runtimeHandler() *adminruntime.Handler {
	return &adminruntime.Handler{Service: a.runtimeState.service, Runtime: a.Engine, Pools: a.runtimeState.pools, Network: a.Config.Network, Audit: a.runtimeAudit}
}

func (a *App) accountPoolsHandler() *accountpools.Handler {
	h := &accountpools.Handler{Pools: a.runtimeState.pools, Policy: a.runtimeState.service, Runtime: a.Engine, Audit: a.runtimeAudit}
	if a.BrowserIdentities() != nil {
		h.IdentityView = a.diagnosticHandler().View
	}
	return h
}

func (a *App) accountsHandler() *accounts.Handler {
	h := &accounts.Handler{Audit: a.runtimeAudit, Runtime: a.Engine.AccountStatus}
	if a.accountOnboarding != nil {
		h.Service = a.accountOnboarding
	}
	return h
}
