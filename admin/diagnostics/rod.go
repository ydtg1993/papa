package diagnostics

import (
	"errors"
	"github.com/ydtg1993/papa/v4/admin/internal/httpjson"
	"github.com/ydtg1993/papa/v4/internal/browserdiagnostics"
	"net/http"
)

func (a *Handler) Rod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	report, err := a.Service.Rod(r.Context())
	if err != nil {
		status := 503
		if errors.Is(err, browserdiagnostics.ErrBusy) {
			status = 409
		}
		http.Error(w, err.Error(), status)
		return
	}
	httpjson.Write(w, 200, report)
}
