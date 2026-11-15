package server

import (
	"errors"
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExecutionTraceComposition(t *testing.T) {
	m := NewMonitor(fakeStageStats, testLogger{t: t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, TaskTrace: func(id int) ([]core.TraceExecutionStep, error) {
		if id != 7 {
			t.Fatalf("id=%d", id)
		}
		return []core.TraceExecutionStep{{TraceStep: core.TraceStep{Step: "parse"}, ExecutionID: "run-7", Truncated: true}}, nil
	}})
	rr := serve(m, http.MethodGet, "/api/task/trace?id=7", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	steps := body["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("steps=%v", steps)
	}
	step := steps[0].(map[string]any)
	if step["execution_id"] != "run-7" || step["truncated"] != true || step["step"] != "parse" {
		t.Fatalf("step=%v", step)
	}
}

func TestTaskTraceHandler(t *testing.T) {
	steps := []core.TraceExecutionStep{
		{TraceStep: core.TraceStep{Attempt: 0, Seq: 0, Step: "打开列表页", Status: "ok", Duration: 12 * time.Millisecond}},
		{TraceStep: core.TraceStep{Attempt: 0, Seq: 1, Step: "解析详情", Status: "failed", Kind: "no-retry", Message: "selector not found"}},
	}
	withTrace := NewMonitor(fakeStageStats, testLogger{t: t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		TaskTrace: func(id int) ([]core.TraceExecutionStep, error) {
			if id != 7 {
				t.Errorf("handler 透传的 id = %d, want 7", id)
			}
			return steps, nil
		},
	})

	t.Run("正常返回步骤", func(t *testing.T) {
		rr := serve(withTrace, http.MethodGet, "/api/task/trace?id=7", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["task_id"].(float64) != 7 {
			t.Fatalf("task_id = %v", body["task_id"])
		}
		got, ok := body["steps"].([]any)
		if !ok || len(got) != 2 {
			t.Fatalf("steps = %#v", body["steps"])
		}
		first := got[0].(map[string]any)
		if first["step"] != "打开列表页" || first["status"] != "ok" {
			t.Errorf("第一步 = %#v", first)
		}
		if second := got[1].(map[string]any); second["status"] != "failed" || second["message"] != "selector not found" {
			t.Errorf("第二步 = %#v", second)
		}
	})

	// 业务原因（追踪未开启）要原样回给前端，不能糊成 500
	t.Run("业务错误原样带回", func(t *testing.T) {
		m := NewMonitor(fakeStageStats, testLogger{t: t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			TaskTrace: func(int) ([]core.TraceExecutionStep, error) {
				return nil, errors.New("步骤追踪未开启（core.trace.enabled）")
			},
		})
		rr := serve(m, http.MethodGet, "/api/task/trace?id=7", nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "步骤追踪未开启") {
			t.Fatalf("body = %q, 应带上服务端那句原因", rr.Body.String())
		}
	})

	t.Run("非法 id", func(t *testing.T) {
		for _, target := range []string{"/api/task/trace", "/api/task/trace?id=abc", "/api/task/trace?id=0", "/api/task/trace?id=-1"} {
			if rr := serve(withTrace, http.MethodGet, target, nil); rr.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", target, rr.Code)
			}
		}
	})

	t.Run("非 GET 拒绝", func(t *testing.T) {
		if rr := serve(withTrace, http.MethodPost, "/api/task/trace?id=7", nil); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rr.Code)
		}
	})

	t.Run("未配置时 404", func(t *testing.T) {
		if rr := serve(newFakeMonitor(t), http.MethodGet, "/api/task/trace?id=7", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})
}
