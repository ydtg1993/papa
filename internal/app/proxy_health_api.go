package app

import "net/http"

func (a *App) proxyHealthAPI(w http.ResponseWriter, r *http.Request) {
	a.runtimeHandler().ProxyHealth(w, r)
}
