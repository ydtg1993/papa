package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

type testRuleAdmin struct {
	RuleAdministration
	calls   int
	failure error
	actor   rulestore.Actor
	request rulestore.RuleChangeRequest
}

func (a *testRuleAdmin) Change(ctx context.Context, p rulestore.RuleChangeRequest, actor rulestore.Actor) (rulestore.ReleaseView, error) {
	a.calls++
	a.actor = actor
	a.request = p
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded request")
	}
	return rulestore.ReleaseView{ID: "release", OperationID: p.OperationID, Generation: "9007199254740993"}, a.failure
}
func (a *testRuleAdmin) Releases(ctx context.Context, site string, before uint64) (rulestore.ReleasePage, error) {
	a.calls++
	return rulestore.ReleasePage{Current: &rulestore.ReleaseView{Generation: "9007199254740993"}, Items: []rulestore.ReleaseView{}}, a.failure
}
func TestRuleAdminHTTPGuardsAuditAndPrecision(t *testing.T) {
	a := &testRuleAdmin{}
	operator := "alice"
	var events []RuleChangeEvent
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{RuleAdmin: a, VerifyToken: func(r *http.Request) (string, bool) { return operator, r.Header.Get("Authorization") == "Bearer valid" }, OnRuleChange: func(e RuleChangeEvent) { events = append(events, e) }})
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
	body := `{"kind":"publish","site":"s","generation":"9007199254740992","bindings":{"list":{"id":"r","version":"v1"}},"verification_ids":{"list":"proof"},"operation_id":"op"}`
	for _, test := range []struct {
		method, path, body string
		auth               bool
		code               int
	}{
		{"POST", "/api/rules/change", body, false, 401}, {"GET", "/api/rules/change", "", true, 405},
		{"POST", "/api/rules/change", strings.TrimSuffix(body, "}") + `,"operator":"spoof"}`, true, 400},
		{"POST", "/api/rules/change", body + ` {}`, true, 400},
		{"POST", "/api/rules/change", strings.Replace(body, `"9007199254740992"`, `9007199254740992`, 1), true, 400},
		{"POST", "/api/rules/preview", `{"kind":"rollback","site":"s","generation":"0","bindings":{"x":{"id":"r","version":"1"}}}`, true, 400},
		{"GET", "/api/rules/releases?site=s&before=-1", "", true, 400},
		{"GET", "/api/rules/version?id=x", "", true, 400},
		{"POST", "/api/rules/change", strings.Repeat(" ", 65<<10), true, 400},
	} {
		w := request(test.method, test.path, test.body, test.auth)
		if w.Code != test.code {
			t.Fatalf("%s %s: %d %s", test.method, test.path, w.Code, w.Body)
		}
	}
	if a.calls != 0 {
		t.Fatal("invalid request reached repository")
	}
	operator = ""
	if w := request("POST", "/api/rules/change", body, true); w.Code != 403 {
		t.Fatalf("anonymous write: %d", w.Code)
	}
	operator = "alice"
	w := request("POST", "/api/rules/change", body, true)
	if w.Code != 200 || a.actor != (rulestore.Actor{Operator: "alice", Source: "oa"}) || len(events) != 1 || events[0].Operator != "alice" || !strings.Contains(w.Body.String(), `"generation":"9007199254740993"`) {
		t.Fatalf("write/audit: %d %s %+v %+v", w.Code, w.Body, a.actor, events)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable rule response")
	}
	for _, test := range []struct {
		err  error
		code int
	}{{rulestore.ErrReviewConflict, 409}, {rulestore.ErrGenerationConflict, 409}, {rulestore.ErrOperationConflict, 409}, {context.DeadlineExceeded, 504}, {errors.New("SQL secret DSN"), 500}} {
		a.failure = test.err
		w = request("POST", "/api/rules/change", body, true)
		if w.Code != test.code || strings.Contains(w.Body.String(), "SQL secret") {
			t.Fatalf("error mapping: %d %s", w.Code, w.Body)
		}
	}
	if len(events) != 6 || events[5].Err == nil {
		t.Fatal("failed attempts not audited")
	}
	a.failure = rulestore.ErrCorrupt
	w = request("POST", "/api/rules/change", body, true)
	if w.Header().Get("X-Rule-Outcome") != "" {
		t.Fatal("post-commit integrity failure marked rejected")
	}
	a.failure = rulestore.ErrReviewConflict
	w = request("POST", "/api/rules/change", body, true)
	if w.Header().Get("X-Rule-Outcome") != "rejected" {
		t.Fatal("definite stale review not distinguished")
	}
}
