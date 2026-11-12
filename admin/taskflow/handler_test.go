package taskflow

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyValidationPrecedesCASAndAudit(t *testing.T) {
	calls, audits := 0, 0
	h := New(Options{TaskFlow: func(context.Context, string) (engine.TaskFlowView, error) { return engine.TaskFlowView{}, nil }, UpdateTaskFlow: func(context.Context, string, uint64, engine.FlowPolicy) error { calls++; return engine.ErrFlowConflict }, OnTaskFlowAction: func(e TaskFlowActionEvent) {
		audits++
		if e.Err != engine.ErrFlowConflict || e.Operator != "alice" {
			t.Error(e)
		}
	}})
	body := `{"revision":2,"policy":{"stages":{"detail":"items"},"lanes":{"items":{"mode":"depth","parallel":1}}}}`
	for _, tc := range []struct {
		body string
		want int
	}{{strings.Replace(body, `"revision":2`, `"revision":2,"revision":3`, 1), 400}, {body, 409}} {
		r := httptest.NewRequest("POST", "/api/task-flow?id=run", strings.NewReader(tc.body))
		r = r.WithContext(auth.WithOperator(r.Context(), "alice"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 1 || audits != 1 {
		t.Fatal(calls, audits)
	}
}
