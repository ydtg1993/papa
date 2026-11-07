package server

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

func TestDefaultAuthProtectsManagementAndExtensionRoutes(t *testing.T) {
	for _, state := range []string{"missing-verifier", "empty-store", "all-disabled", "lookup-error"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			cfg := MonitorConfig{OnShutdown: func() { calls++ }, VerifyTokenValue: func(string) (string, bool) { calls++; return "unreachable", true }}
			if state != "missing-verifier" {
				f := gormfake.New()
				f.SetCounts(0, 0)
				if state == "all-disabled" {
					f.SetCounts(0, 2)
				}
				if state == "lookup-error" {
					f.SetQueryError("token_hash", "database unavailable")
				}
				counter := auth.NewTokenCounter(gormfake.MustOpen(f), time.Hour)
				cfg.VerifyToken = counter.Verifier(nil)
				cfg.AuthRequired = counter.Required
			}
			m := NewMonitor(emptyGetter, testLogger{t}, cfg)
			mux := http.NewServeMux()
			m.Register(mux)
			for _, path := range []string{"/api/tokens", "/api/oao/task/action/retry", "/business-private"} {
				mux.Handle(path, m.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) })))
			}
			for _, path := range []string{"/api/monitor", "/api/settings/shutdown", "/api/tokens", "/api/oao/task/action/retry", "/business-private"} {
				for _, credential := range []string{"", "Bearer arbitrary"} {
					r := httptest.NewRequest("POST", path, strings.NewReader(`{"token":"arbitrary"}`))
					if path == "/api/monitor" {
						r.Method = "GET"
					}
					r.Header.Set("Authorization", credential)
					r.Header.Set("X-Forwarded-For", "127.0.0.1")
					r.RemoteAddr = "127.0.0.1:12345"
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("WWW-Authenticate") == "" {
						t.Fatalf("%s: status=%d headers=%v", path, w.Code, w.Header())
					}
				}
			}
			if calls != 0 {
				t.Fatalf("unauthorized handler side effects=%d", calls)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/monitor", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `data-auth-required="1"`) {
				t.Fatal("login shell unavailable or anonymously initialized")
			}
		})
	}
}

func TestFirstTokenImmediatelyAuthenticatesAndRevocationImmediatelyDenies(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0)
	counter := auth.NewTokenCounter(gormfake.MustOpen(f), time.Hour)
	counter.CountEnabled()
	counter.CountAll()
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: counter.Verifier(nil)})
	var actor string
	h := m.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor = auth.OperatorFrom(r.Context())
		w.WriteHeader(200)
	}))
	request := func() int {
		r := httptest.NewRequest("GET", "/business-private", nil)
		r.Header.Set("Authorization", "Bearer new-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if request() != 401 {
		t.Fatal("empty store allowed request")
	}
	f.SetRow(map[string]driver.Value{"operator": "alice"})
	if request() != 200 || actor != "alice" {
		t.Fatal("first token did not authenticate immediately")
	}
	f.SetRow(nil)
	actor = ""
	if request() != 401 || actor != "" {
		t.Fatal("revoked token still executed handler")
	}
}

func TestLoginDisplayOverrideDoesNotAuthorizeRequests(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{AuthRequired: func() bool { return false }})
	if rr := serve(m, "GET", "/api/monitor", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("login display override authorized API: status=%d", rr.Code)
	}
	called := false
	w := httptest.NewRecorder()
	m.Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(w, httptest.NewRequest("GET", "/private", nil))
	if w.Code != http.StatusUnauthorized || called {
		t.Fatal("login display override authorized extension")
	}
}
