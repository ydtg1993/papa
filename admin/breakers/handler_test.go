package breakers

import (
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/core"
	"net/http/httptest"
	"strings"
	"testing"
)

type silentLogger struct{}

func (silentLogger) Info(...any)           {}
func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}
func TestResumeValidatesTargetAndAuditsChangedSites(t *testing.T) {
	var calls, audits []string
	h := New(Options{BreakerStatuses: func() map[string]core.BreakerStatus { return map[string]core.BreakerStatus{"b": {}, "a": {}} }, ResumeBreaker: func(site, stage string) bool { calls = append(calls, site); return site == "a" }, OnBreakerResume: func(op, site, stage string) { audits = append(audits, op+":"+site) }}, silentLogger{})
	r := httptest.NewRequest("POST", "/api/breaker/resume?stage=detail", nil)
	w := httptest.NewRecorder()
	h.Resume(w, r)
	if w.Code != 400 || len(calls) != 0 {
		t.Fatal("invalid stage altered protection")
	}
	r = httptest.NewRequest("POST", "/api/breaker/resume", nil)
	r = r.WithContext(auth.WithOperator(r.Context(), "alice"))
	w = httptest.NewRecorder()
	h.Resume(w, r)
	if w.Code != 200 || strings.Join(calls, ",") != "a,b" || strings.Join(audits, ",") != "alice:a" {
		t.Fatal(w.Code, calls, audits)
	}
}
