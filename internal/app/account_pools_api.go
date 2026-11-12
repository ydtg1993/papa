package app

import (
	"github.com/ydtg1993/papa/v4/admin/accountpools"
	"net/http"
)

var accountPoolsScript = accountpools.Script

func (a *App) accountPoolsHandler() *accountpools.Handler {
	h := &accountpools.Handler{Pools: a.runtimeState.pools, Policy: a.runtimeState.service, Runtime: a.Engine, Audit: a.runtimeAudit}
	if a.BrowserIdentities() != nil {
		h.IdentityView = a.identityView
	}
	return h
}
func (a *App) accountPoolsAPI(w http.ResponseWriter, r *http.Request) {
	a.accountPoolsHandler().PoolsAPI(w, r)
}
