package runtimecontrol

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/core"
	domain "github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type policyStub struct {
	calls    int
	input    domain.Update
	operator string
	out      domain.Result
}

func (s *policyStub) Read(context.Context) domain.Result { s.calls++; return s.out }
func (s *policyStub) Save(_ context.Context, v domain.Update, op string) domain.Result {
	s.calls++
	s.input = v
	s.operator = op
	return s.out
}
func (s *policyStub) Acknowledge(string, uint64) domain.Result { s.calls++; return s.out }
func (s *policyStub) History(context.Context) domain.Result    { s.calls++; return s.out }
func (s *policyStub) HistoryAction(context.Context, domain.HistoryRequest, string) domain.Result {
	s.calls++
	return s.out
}
func TestPolicyAdapterRejectsMalformedInputsBeforeService(t *testing.T) {
	for _, body := range []string{`{"revision":1,"unknown":true}`, `{} {}`, strings.Repeat(" ", 129<<10) + `{}`} {
		s := &policyStub{}
		h := &Handler{Service: s}
		w := httptest.NewRecorder()
		h.Policy(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if w.Code != 400 || s.calls != 0 {
			t.Fatal("invalid request reached domain", w.Code, s.calls)
		}
	}
}
func TestPolicyAdapterPreservesDomainConflictAndExplicitOperator(t *testing.T) {
	s := &policyStub{out: domain.Result{Kind: domain.Conflict, Value: map[string]any{"error": "conflict"}, Events: []domain.Event{{Action: "save", ID: "7"}}}}
	events := 0
	h := &Handler{Service: s, Audit: func(_ *http.Request, table, action, id string, _ error) {
		if table != "runtime_policy" || action != "save" || id != "7" {
			t.Fatal(table, action, id)
		}
		events++
	}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"revision":6,"document":{"schema_version":1,"overrides":{}}}`))
	r = r.WithContext(auth.WithOperator(r.Context(), "operator"))
	w := httptest.NewRecorder()
	h.Policy(w, r)
	if w.Code != 409 || s.input.Revision != 6 || s.operator != "operator" || events != 1 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, s, events)
	}
}
func TestHistoryAdapterRejectsUnknownAction(t *testing.T) {
	s := &policyStub{}
	h := &Handler{Service: s}
	w := httptest.NewRecorder()
	h.History(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"action":"delete"}`)))
	if w.Code != 400 || s.calls != 0 {
		t.Fatal(w.Code, s.calls)
	}
}

// The live controller can advance after the service captures a response.
type metricsRuntime struct {
	Runtime
	controller *governance.Controller
}

func (*metricsRuntime) GetSiteStats() map[string]core.SiteStat         { return nil }
func (*metricsRuntime) GetStageStats() map[string]core.StageStats      { return nil }
func (*metricsRuntime) RuntimeDeclarations() map[string]map[string]any { return nil }
func (*metricsRuntime) GetBrowserPool() *browser.Pool                  { return nil }
func (*metricsRuntime) GetProxy() *proxy.Manager                       { return nil }
func (r *metricsRuntime) Governance() *governance.Controller           { return r.controller }

func TestPolicyEnrichmentPreservesRevisionBoundSnapshot(t *testing.T) {
	c, err := governance.New(governance.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Runtime: &metricsRuntime{controller: c}}
	// These values came from one publication transaction, before live state advanced.
	v := map[string]any{"revision": uint64(1), "snapshot": "captured-governance", "proxies": "captured-proxies", "pools": "captured-pools"}
	h.enrich(v)
	if v["snapshot"] != "captured-governance" || v["proxies"] != "captured-proxies" || v["pools"] != "captured-pools" {
		t.Fatalf("HTTP enrichment replaced a revision-bound snapshot: %+v", v)
	}
}
