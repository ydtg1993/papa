package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
)

// 热更那条入口单独校验：它绕过 config.Load（LoadRuntime 只 yaml.Unmarshal，
// ApplyRuntimeConfig 只 Merge + Store）。越界值被拦在 handler 里、回 400 而不是 500 ——
// 那是调用方的输入错，不是服务端故障；而且一次都不该下发。
//
// 判据本身（config.ValidateRuntime）在 config 包里有单元测试，这里只钉"接线"：
// 拦得住、状态码对、ConfigSet 没被调。
func TestConfigPutRejectsOutOfRangeRuntimeValue(t *testing.T) {
	applied := 0
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ConfigGet: func() *config.RuntimeConfig { return &config.RuntimeConfig{} },
			ConfigSet: func(*config.RuntimeConfig) error {
				applied++
				return nil
			},
		})
	}

	for _, c := range []struct{ name, body string }{
		{"max_body_size 写 0", `{"html":{"max_body_size":0}}`},
		{"max_body_size 写 10（想当 10MB）", `{"html":{"max_body_size":10}}`},
		{"max_body_size 超上界 64MB", `{"html":{"max_body_size":67108865}}`},
		{"error_queue.worker_count 负数", `{"error_queue":{"worker_count":-1}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := serve(newMon(), http.MethodPut, "/api/config", strings.NewReader(c.body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（body: %s）", rr.Code, rr.Body.String())
			}
			if applied != 0 {
				t.Fatalf("越界时不该下发，ConfigSet 实调 %d 次", applied)
			}
		})
	}

	// 合法值照旧下发 —— 防止把校验写成了"一律拒绝"
	rr := serve(newMon(), http.MethodPut, "/api/config", strings.NewReader(`{"html":{"max_body_size":10485760}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("合法值应当 200，实得 %d（body: %s）", rr.Code, rr.Body.String())
	}
	if applied != 1 {
		t.Fatalf("合法值应当下发一次，实得 %d", applied)
	}
}
