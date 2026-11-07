package server

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testAgentAdmin struct {
	AgentAdministration
	calls    int
	operator string
	failure  error
	input    agentadmin.StartInput
}

func (a *testAgentAdmin) Start(ctx context.Context, in agentadmin.StartInput, operator string) (agentadmin.RequestView, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded")
	}
	a.calls++
	a.operator = operator
	a.input = in
	return agentadmin.RequestView{ID: in.ID, Revision: in.Revision}, a.failure
}

type testQualityWriter struct {
	QualityAdministration
	in       rulequality.FreezeInput
	operator string
}

func (a *testQualityWriter) Freeze(ctx context.Context, in rulequality.FreezeInput, operator string) (rulequality.Report, error) {
	a.in = in
	a.operator = operator
	return rulequality.Report{ID: "frozen", OperationID: in.OperationID, Query: in.Query}, nil
}
func (a *testQualityWriter) ReportOperation(context.Context, string) (rulequality.Report, error) {
	return rulequality.Report{}, rulequality.ErrNotFound
}
func TestAgentHTTPNamedGuardsDuplicateKeysUnknownOutcomeAndAudit(t *testing.T) {
	a := &testAgentAdmin{}
	quality := &testQualityWriter{}
	operator := "alice"
	events := []AgentActionEvent{}
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{AgentAdmin: a, QualityAdmin: quality, VerifyToken: func(r *http.Request) (string, bool) { return operator, r.Header.Get("Authorization") == "Bearer valid" }, OnAgentAction: func(e AgentActionEvent) { events = append(events, e) }})
	mux := http.NewServeMux()
	mon.Register(mux)
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer valid")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	body := `{"report_id":"report","profile_id":"approved","evidence_ids":["evidence"],"digest":"sha","id":"request","revision":"9007199254740993"}`
	for _, tt := range []struct {
		method, path, body string
		auth               bool
		code               int
	}{{"POST", "/api/agents/start", body, false, 401}, {"GET", "/api/agents/start", "", true, 405}, {"POST", "/api/agents/start", body + `{}`, true, 400}, {"POST", "/api/agents/start", strings.Replace(body, `"sha"`, `"sha","digest":"override"`, 1), true, 400}, {"POST", "/api/agents/start", strings.Replace(body, `"sha"`, `"sha","operator":"spoof"`, 1), true, 400}, {"POST", "/api/agents/start", strings.Replace(body, `"9007199254740993"`, `9007199254740993`, 1), true, 400}, {"POST", "/api/agents/start", strings.Repeat(" ", 65<<10), true, 400}, {"GET", "/api/agents/run?id=a&id=b", "", true, 400}, {"POST", "/api/agents/start?override=1", body, true, 400}, {"GET", "/api/agents/runs?from=no&until=no", "", true, 400}} {
		w := request(tt.method, tt.path, tt.body, tt.auth)
		if w.Code != tt.code {
			t.Fatalf("%s %s: %d %s", tt.method, tt.path, w.Code, w.Body)
		}
	}
	if a.calls != 0 {
		t.Fatal("invalid request reached worker")
	}
	operator = ""
	if w := request("POST", "/api/agents/start", body, true); w.Code != 403 {
		t.Fatal(w.Code)
	}
	operator = "alice"
	w := request("POST", "/api/agents/start", body, true)
	if w.Code != 202 || a.operator != "alice" || a.input.Revision != "9007199254740993" || len(events) != 1 || events[0].ID != "request" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("accepted %+v %s", events, w.Body)
	}
	a.failure = errors.New("secret SQL DSN provider reply")
	w = request("POST", "/api/agents/start", body, true)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
	freeze := `{"operation_id":"op","input_sha256":"digest","query":{"scope":{"site":"s","stage":"list","rule_id":"r","version":"v","release_id":"rel","release_generation":"9007199254740993"},"from":"2026-11-06T00:00:00Z","until":"2026-11-07T00:00:00Z","limit":1000,"lost_observations":0}}`
	w = request("POST", "/api/quality/freeze", freeze, true)
	if w.Code != 200 || quality.operator != "alice" || quality.in.Query.Scope.ReleaseGeneration != 9007199254740993 || !strings.Contains(w.Body.String(), `"release_generation":"9007199254740993"`) {
		t.Fatalf("freeze %d %s", w.Code, w.Body)
	}
}
