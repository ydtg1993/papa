package server

import (
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"strings"
	"testing"
)

func TestBreakerRetryAndForceAreSeparateEndpoints(t *testing.T) {
	retries, forces, audited := 0, 0, 0
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		BreakerStatuses: func() map[string]core.BreakerStatus {
			return map[string]core.BreakerStatus{"fixture": {Site: "fixture", Paused: true}}
		},
		ResumeBreaker:        func(site, stage string) bool { retries++; return true },
		ForceResumeBreaker:   func(site string) bool { forces++; return true },
		OnBreakerForceResume: func(operator, site string) { audited++ },
	})
	r := serve(m, http.MethodPost, "/api/breaker/resume", strings.NewReader(`{"scope":"fixture"}`))
	if r.Code != 200 || retries != 1 || forces != 0 {
		t.Fatalf("ordinary resume invoked force: %d %s", r.Code, r.Body.String())
	}
	for _, body := range []string{`{}`, `{"scope":"fixture","stage":"detail"}`, `{"scope":"unknown"}`} {
		r = serve(m, http.MethodPost, "/api/breaker/force-resume", strings.NewReader(body))
		if r.Code != 400 || forces != 0 {
			t.Fatal("force endpoint accepted ambiguous scope")
		}
	}
	r = serve(m, http.MethodPost, "/api/breaker/force-resume", strings.NewReader(`{"scope":"fixture"}`))
	if r.Code != 200 || retries != 1 || forces != 1 || audited != 1 || !strings.Contains(r.Body.String(), `"mode":"force"`) {
		t.Fatalf("force path/audit: %d %s", r.Code, r.Body.String())
	}
}
