package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/admin/server"
	"github.com/ydtg1993/papa/v4/core"
)

type routeLogger struct{}

func (routeLogger) Info(...any)           {}
func (routeLogger) Infof(string, ...any)  {}
func (routeLogger) Errorf(string, ...any) {}

// Feature extraction must not accidentally mount an endpoint outside the shared
// access gate. A rejected request must not touch feature dependencies.
func TestAllFeatureRoutesShareMonitorAccessGate(t *testing.T) {
	routes := []string{
		"/api/monitor", "/api/task-flow", "/api/settings", "/api/settings/whitelist", "/api/settings/shutdown",
		"/api/errorqueue/process", "/api/repeatqueue/process", "/api/breaker", "/api/breaker/analysis",
		"/api/breaker/resume", "/api/breaker/force-resume", "/api/breaker/pause", "/api/task/trace",
		"/api/logs", "/api/logs/download", "/api/logs/tail", "/api/task/page", "/api/obs/reports", "/api/obs/report",
	}
	for _, mode := range []string{"source", "token"} {
		t.Run(mode, func(t *testing.T) {
			cfg := server.MonitorConfig{}
			want := http.StatusUnauthorized
			if mode == "source" {
				cfg.Whitelist = []string{"10.0.0.0/8"}
				cfg.VerifyToken = func(*http.Request) (string, bool) { t.Error("source denial reached token verifier"); return "", false }
				want = http.StatusForbidden
			}
			monitor := server.NewMonitor(func() map[string]core.StageStats { t.Error("denied request reached monitoring"); return nil }, routeLogger{}, cfg)
			mux := http.NewServeMux()
			monitor.Register(mux)
			for _, path := range routes {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					t.Run(method+" "+path, func(t *testing.T) {
						req := httptest.NewRequest(method, path, nil)
						req.RemoteAddr = "192.0.2.1:1234"
						w := httptest.NewRecorder()
						mux.ServeHTTP(w, req)
						if w.Code != want {
							t.Fatalf("got %d, want %d: %s", w.Code, want, w.Body.String())
						}
					})
				}
			}
		})
	}
}

// Resource ownership may move, but deployed templates and business extensions
// still use these URLs. Check the actual mux, not only an embedded file list.
func TestLegacyAssetURLsRemainAvailable(t *testing.T) {
	mux := http.NewServeMux()
	server.NewMonitor(func() map[string]core.StageStats { return nil }, routeLogger{}, server.MonitorConfig{}).Register(mux)
	for _, name := range []string{
		"mo.js", "mo.css", "workbench.js", "workbench.css", "visuals.js", "visuals.css", "visual-state.js",
		"trace.js", "table-links.js", "requests.js", "queue-governance.js", "queue-governance.css", "navigation.js",
		"layout.css", "extensions.js", "appearance.js", "fonts/space-grotesk-latin.woff2", "fonts/archivo-black-latin.woff2",
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Fatalf("legacy asset unavailable: status=%d bytes=%d", w.Code, w.Body.Len())
			}
			if strings.HasSuffix(name, ".js") && !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
				t.Fatalf("script served with wrong MIME: %s", w.Header().Get("Content-Type"))
			}
			if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatalf("asset lost cache policy: %s", w.Header().Get("Cache-Control"))
			}
		})
	}
}
