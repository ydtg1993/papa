package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/obs/incident"
)

func TestBreakerAnalysisAPI(t *testing.T) {
	now := time.Now()
	m := newFakeMonitor(t)
	m.cfg.BreakerStatuses = func() map[string]core.BreakerStatus { return map[string]core.BreakerStatus{"a": {Site: "a"}} }
	m.cfg.BreakerIncidents = func(site string) []incident.Capture {
		return []incident.Capture{{ID: "one", Site: site, At: now, From: now.Add(-time.Minute), Failures: 1, Samples: []incident.Sample{{At: now, Error: "HTTP 429 token=hidden-value"}}}}
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/breaker/analysis?scope=a", 200}, {"GET", "/api/breaker/analysis?scope=a&id=one&format=text", 200},
		{"GET", "/api/breaker/analysis?scope=a&id=one&format=json", 200}, {"GET", "/api/breaker/analysis?scope=a&id=missing", 404},
		{"GET", "/api/breaker/analysis", 400}, {"GET", "/api/breaker/analysis?scope=b", 400}, {"POST", "/api/breaker/analysis?scope=a", 405},
		{"GET", "/api/breaker/analysis?scope=a&format=html", 400},
	} {
		rr := serve(m, tc.method, tc.path, nil)
		if rr.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "hidden-value") {
			t.Fatal("credential leak")
		}
	}
	m.cfg.BreakerIncidents = func(string) []incident.Capture { return nil }
	if rr := serve(m, "GET", "/api/breaker/analysis?scope=a", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"reports":[]`) {
		t.Fatalf("empty: %s", rr.Body.String())
	}
	m.cfg.VerifyToken = func(*http.Request) (string, bool) { return "", false }
	if rr := serve(m, "GET", "/api/breaker/analysis?scope=a", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated access: %d", rr.Code)
	}
}
