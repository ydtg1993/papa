package server

import (
	"context"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskFlowHTTPExplicitCASAndAuthentication(t *testing.T) {
	calls := 0
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) {
			return "operator", r.Header.Get("Authorization") == "Bearer test"
		},
		TaskFlow: func(context.Context, string) (engine.TaskFlowView, error) { return engine.TaskFlowView{}, nil },
		UpdateTaskFlow: func(_ context.Context, id string, rev uint64, p engine.FlowPolicy) error {
			calls++
			if id != "run" || rev != 2 || p.Lanes["items"].Mode != "depth" {
				t.Error("wrong arguments")
			}
			return engine.ErrFlowConflict
		},
	})
	mux := http.NewServeMux()
	mon.Register(mux)
	body := `{"revision":2,"policy":{"stages":{"detail":"items"},"lanes":{"items":{"mode":"depth","parallel":1}}}}`
	for _, tc := range []struct {
		method, body string
		auth         bool
		want         int
	}{
		{"POST", body, false, 401}, {"GET", "", true, 200}, {"DELETE", "", true, 405},
		{"POST", `{"revision":0}`, true, 400},
		{"POST", strings.Replace(body, `"revision":2`, `"revision":2,"revision":3`, 1), true, 400},
		{"POST", strings.Replace(body, `"parallel":1`, `"parallel":1,"parallel":2`, 1), true, 400},
		{"POST", body + `{}`, true, 400},
		{"POST", body + strings.Repeat(" ", 1<<20), true, 400},
		{"POST", strings.Replace(body, `"revision":2`, `"revision":2,"unknown":true`, 1), true, 400},
		{"POST", body, true, 409},
	} {
		r := httptest.NewRequest(tc.method, "/api/task-flow?id=run", strings.NewReader(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer test")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s status %d want %d", tc.method, w.Code, tc.want)
		}
	}
	if calls != 1 {
		t.Fatal("write invoked without validated explicit request", calls)
	}
}
