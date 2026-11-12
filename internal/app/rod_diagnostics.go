package app

import (
	"github.com/ydtg1993/papa/v4/admin/diagnostics"
	"net/http"
)

// RodDiagnostics preserves the public route factory; the adapter owns HTTP.
func RodDiagnostics(a *App) http.HandlerFunc {
	return (&diagnostics.Handler{Service: a.diagnosticService()}).Rod
}
