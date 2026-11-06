package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

type testQualityAdmin struct {
	QualityAdministration
	calls   int
	failure error
	filter  rulequality.AdminFilter
	query   rulequality.Query
}

func (a *testQualityAdmin) Observations(ctx context.Context, f rulequality.AdminFilter) (rulequality.ObservationPage, error) {
	a.calls++
	a.filter = f
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded query")
	}
	return rulequality.ObservationPage{Cutoff: 9007199254740993, Next: 9007199254740992, Items: []rulequality.Observation{{Sequence: 9007199254740993, Scope: rulequality.Scope{ReleaseGeneration: 9007199254740993}}}}, a.failure
}
func (a *testQualityAdmin) Summary(ctx context.Context, q rulequality.Query) (rulequality.Report, error) {
	a.calls++
	a.query = q
	return rulequality.Report{}, a.failure
}
func TestQualityHTTPGuardsPrecisionAndErrors(t *testing.T) {
	a := &testQualityAdmin{}
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{QualityAdmin: a, VerifyToken: func(r *http.Request) (string, bool) { return "alice", r.Header.Get("Authorization") == "Bearer valid" }})
	mux := http.NewServeMux()
	mon.Register(mux)
	request := func(method, path string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer valid")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	window := "from=2026-11-01T00:00:00Z&until=2026-11-02T00:00:00Z"
	for _, tc := range []struct {
		method, path string
		auth         bool
		code         int
	}{
		{"GET", "/api/quality/observations?" + window, false, 401}, {"POST", "/api/quality/observations?" + window, true, 405},
		{"GET", "/api/quality/observations", true, 400}, {"GET", "/api/quality/observations?" + window + "&before=1", true, 400},
		{"GET", "/api/quality/observations?" + window + "&cutoff=01", true, 400}, {"GET", "/api/quality/observations?" + window + "&cutoff=18446744073709551616", true, 400},
		{"GET", "/api/quality/observations?" + window + "&site=a&site=b", true, 400}, {"GET", "/api/quality/observations?" + window + "&unknown=1", true, 400},
		{"GET", "/api/quality/observations?from=2026-10-01T00:00:00Z&until=2026-11-01T00:00:00Z", true, 400},
		{"GET", "/api/quality/summary?" + window, true, 400}, {"GET", "/api/quality/summary?" + window + "&site=s&stage=c&rule_id=r&version=v&release_generation=2", true, 400},
		{"GET", "/api/quality/report", true, 400}, {"GET", "/api/quality/write", true, 404},
	} {
		w := request(tc.method, tc.path, tc.auth)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	if a.calls != 0 {
		t.Fatal("invalid request reached store")
	}
	w := request("GET", "/api/quality/observations?"+window+"&cutoff=9007199254740993&before=9007199254740992", true)
	if w.Code != 200 || a.filter.Cutoff != 9007199254740993 || !strings.Contains(w.Body.String(), `"sequence":"9007199254740993"`) || !strings.Contains(w.Body.String(), `"release_generation":"9007199254740993"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("precision: %d %s", w.Code, w.Body)
	}
	w = request("GET", "/api/quality/summary?"+window+"&site=s&stage=c&rule_id=r&version=v&release_id=rel&release_generation=9007199254740993", true)
	if w.Code != 200 || a.query.Scope.ReleaseGeneration != 9007199254740993 || a.query.Limit != 1000 || a.query.LostObservations != 0 {
		t.Fatalf("summary contract: %d %+v", w.Code, a.query)
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{rulequality.ErrNotFound, 404}, {rulequality.ErrCorrupt, 409}, {rulequality.ErrEvidence, 409}, {context.DeadlineExceeded, 504}, {errors.New("SQL secret DSN"), 500}} {
		a.failure = tc.err
		w = request("GET", "/api/quality/observations?"+window, true)
		if w.Code != tc.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("mapped error: %d %s", w.Code, w.Body)
		}
	}
	disabled := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{})
	w = httptest.NewRecorder()
	disabled.qualityHandler(w, httptest.NewRequest("GET", "/api/quality/observations", nil))
	if w.Code != 404 {
		t.Fatal("disabled service became enabled")
	}
}
