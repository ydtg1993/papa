package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
)

func TestConfigHandler(t *testing.T) {
	maxIdle := config.Duration{Duration: 3 * time.Minute}
	current := &config.RuntimeConfig{
		Browser: config.RuntimeBrowserConfig{MaxIdleTime: &maxIdle},
	}
	var applied *config.RuntimeConfig
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		ConfigGet: func() *config.RuntimeConfig { return current },
		ConfigSet: func(rt *config.RuntimeConfig) error {
			applied = rt
			return nil
		},
	})

	// GET 返回当前覆盖层
	rr := serve(m, http.MethodGet, "/api/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rr.Code, rr.Body.String())
	}
	v := decodeJSON(t, rr)
	overrides, ok := v["overrides"].(map[string]any)
	if !ok {
		t.Fatalf("overrides missing: %+v", v)
	}
	browser, ok := overrides["browser"].(map[string]any)
	if !ok {
		t.Fatalf("overrides.browser missing: %+v", overrides)
	}
	if browser["max_idle_time"] != "3m0s" {
		t.Fatalf("max_idle_time = %v, want 3m0s", browser["max_idle_time"])
	}
	if _, ok := v["restart_only_fields"]; !ok {
		t.Fatalf("restart_only_fields missing: %+v", v)
	}

	// PUT 合法：解析时长字符串 + 数值
	rr = serve(m, http.MethodPut, "/api/config", strings.NewReader(`{"browser":{"max_idle_time":"10m"},"error_queue":{"worker_count":2}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if applied == nil || applied.Browser.MaxIdleTime == nil || applied.Browser.MaxIdleTime.Duration != 10*time.Minute {
		t.Fatalf("applied max_idle_time = %+v, want 10m", applied)
	}
	if applied.ErrorQueue.WorkerCount == nil || *applied.ErrorQueue.WorkerCount != 2 {
		t.Fatalf("applied error_queue.worker_count = %+v, want 2", applied.ErrorQueue.WorkerCount)
	}

	// PUT 拒绝重启字段（unknown field）
	rr = serve(m, http.MethodPut, "/api/config", strings.NewReader(`{"browser":{"headless":false}}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("restart field status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}

	// 池大小已非热更字段：传了同样按 unknown field 拒绝
	rr = serve(m, http.MethodPut, "/api/config", strings.NewReader(`{"browser":{"pool_size":7}}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("pool_size status = %d, want 400", rr.Code)
	}
}
