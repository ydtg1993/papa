package server

import (
	"github.com/ydtg1993/papa/v4/admin/shell"
	"net/http"
)

// ExtensionPage is the navigation metadata supplied by a host.
type ExtensionPage = shell.ExtensionPage

// RegisterExtensions preserves the host extension API; the shell owns metadata
// snapshots and delivery of trusted custom resources.
func RegisterExtensions(mux *http.ServeMux, wrap func(http.Handler) http.Handler, pages []ExtensionPage, scripts, styles []string) {
	shell.RegisterExtensions(mux, wrap, pages, scripts, styles)
}
