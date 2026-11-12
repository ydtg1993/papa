package app

import "net/http"

func (a *App) runtimeHistoryAPI(w http.ResponseWriter, r *http.Request) {
	a.runtimeHandler().History(w, r)
}
