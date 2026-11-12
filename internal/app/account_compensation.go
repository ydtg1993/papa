package app

import "net/http"

func (a *App) accountCompensationAPI(w http.ResponseWriter, r *http.Request) {
	a.accountPoolsHandler().Compensation(w, r)
}
