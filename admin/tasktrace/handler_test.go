package tasktrace

import (
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testLogger struct{ calls int }

func (l *testLogger) Errorf(string, ...any) { l.calls++ }
func TestTraceLookupAndErrors(t *testing.T) {
	logger := &testLogger{}
	h := New(Options{Logger: logger, Trace: func(id int) ([]core.TraceExecutionStep, error) {
		if id == 8 {
			return nil, errors.New("trace disabled")
		}
		if id != 7 {
			t.Errorf("unexpected id %d", id)
		}
		return []core.TraceExecutionStep{{TraceStep: core.TraceStep{Step: "parse", Status: "ok"}}}, nil
	}})
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{"GET", "?id=7", 200}, {"GET", "?id=8", 409}, {"GET", "?id=0", 400}, {"GET", "?id=-1", 400}, {"GET", "?id=abc", 400}, {"GET", "", 400}, {"POST", "?id=7", 405},
	} {
		r := httptest.NewRequest(tc.method, "/api/task/trace"+tc.target, nil)
		w := httptest.NewRecorder()
		h.Trace(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.target, w.Code, w.Body.String())
		}
		if w.Code == 409 && !strings.Contains(w.Body.String(), "trace disabled") {
			t.Fatal(w.Body.String())
		}
		if w.Code == 200 {
			var body struct {
				TaskID int                       `json:"task_id"`
				Steps  []core.TraceExecutionStep `json:"steps"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.TaskID != 7 || len(body.Steps) != 1 || body.Steps[0].Step != "parse" {
				t.Fatalf("%+v", body)
			}
		}
	}
	if logger.calls != 1 {
		t.Fatalf("error logs: %d", logger.calls)
	}
	w := httptest.NewRecorder()
	New(Options{}).Trace(w, httptest.NewRequest(http.MethodGet, "/api/task/trace?id=7", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestExecutionTraceLookup(t *testing.T) {
	h := New(Options{Logger: &testLogger{}, Trace: func(id int) ([]core.TraceExecutionStep, error) {
		if id != 7 {
			t.Fatalf("lookup id=%d", id)
		}
		return []core.TraceExecutionStep{{TraceStep: core.TraceStep{Step: "latest", Attempt: 1}, ExecutionID: "run-1", Truncated: true}}, nil
	}})
	w := httptest.NewRecorder()
	h.Trace(w, httptest.NewRequest(http.MethodGet, "/api/task/trace?id=7", nil))
	var response struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(response.Steps) != 1 || response.Steps[0]["step"] != "latest" || response.Steps[0]["execution_id"] != "run-1" || response.Steps[0]["truncated"] != true {
		t.Fatalf("response=%s", w.Body.String())
	}
}
