package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

func TestBreakerAnalysisDurableLifecycle(t *testing.T) {
	m := newFakeMonitor(t)
	m.cfg.BreakerStatuses = func() map[string]core.BreakerStatus { return map[string]core.BreakerStatus{"a": {Site: "a"}} }
	m.cfg.BreakerReports = func(ctx context.Context, site string) ([]incident.Lifecycle, error) {
		if site != "a" || ctx == nil {
			t.Fatal("callback scope")
		}
		return []incident.Lifecycle{
			{Capture: incident.Capture{ID: "one", Site: "a", Failures: 1}, State: "recovered", Revision: 3, Conclusion: "恢复已验证 token=hidden-value", Evidence: []incident.Sample{{Error: "HTTP 503"}}},
			{Capture: incident.Capture{ID: "foreign", Site: "other"}, State: "manual"},
		}, nil
	}
	for _, suffix := range []string{"", "&id=one&format=json", "&id=one&format=text"} {
		rr := serve(m, "GET", "/api/breaker/analysis?scope=a"+suffix, nil)
		if rr.Code != 200 {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "recovered") || !strings.Contains(body, "恢复已验证") || strings.Contains(body, "hidden-value") || strings.Contains(body, "foreign") {
			t.Fatalf("invalid report: %s", body)
		}
	}
	m.cfg.BreakerReports = func(context.Context, string) ([]incident.Lifecycle, error) {
		return nil, errors.New("dsn password=private-value")
	}
	rr := serve(m, "GET", "/api/breaker/analysis?scope=a", nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "private-value") {
		t.Fatalf("store failure: %d %s", rr.Code, rr.Body.String())
	}
}

func TestBreakerAnalysisAPI(t *testing.T) {
	now := time.Now()
	m := newFakeMonitor(t)
	m.cfg.BreakerStatuses = func() map[string]core.BreakerStatus { return map[string]core.BreakerStatus{"a": {Site: "a"}} }
	m.cfg.BreakerReports = func(_ context.Context, site string) ([]incident.Lifecycle, error) {
		return []incident.Lifecycle{{Capture: incident.Capture{ID: "one", Site: site, At: now, From: now.Add(-time.Minute), Failures: 1}, State: "cooldown", Evidence: []incident.Sample{{At: now, Error: "HTTP 429 token=hidden-value"}}}}, nil
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
	m.cfg.BreakerReports = func(context.Context, string) ([]incident.Lifecycle, error) { return nil, nil }
	if rr := serve(m, "GET", "/api/breaker/analysis?scope=a", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"reports":[]`) {
		t.Fatalf("empty: %s", rr.Body.String())
	}
	m.cfg.VerifyToken = nil
	if rr := serve(m, "GET", "/api/breaker/analysis?scope=a", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated access: %d", rr.Code)
	}
}
