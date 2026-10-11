package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ydtg1993/papa/v3/admin/auth"
)

// Monitor.Auth 是给"挂在同一个 mux 上的外部组件"（oao 表格、业务自定义路由）复用的那道闸门 ——
// 业务侧接口绕过鉴权就是从这儿漏的。它以前 0% 覆盖（用例都直接走 s.wrap，没走这个导出口）。
func TestMonitorAuthAppliesGate(t *testing.T) {
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			Whitelist: []string{"10.0.0.0/8"},
			VerifyToken: func(r *http.Request) (string, bool) {
				if auth.Extract(r) == "good" {
					return "张三", true
				}
				return "", false
			},
		})
	}

	// 处理函数：跑到了就把 ctx 里的操作人记下来
	var seenOperator string
	var handlerRan bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRan = true
		seenOperator = auth.OperatorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	call := func(m *Monitor, method, target, token, remote string) *httptest.ResponseRecorder {
		handlerRan, seenOperator = false, ""
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		m.Auth(handler).ServeHTTP(rr, req)
		return rr
	}

	t.Run("白名单外 → 403，处理函数不跑", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/api/oao/thing", "good", "1.2.3.4:1234")
		if rr.Code != http.StatusForbidden || handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 403/false", rr.Code, handlerRan)
		}
	})
	t.Run("白名单内 + /api/ + 令牌不对 → 401 且 no-store，处理函数不跑", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/api/oao/thing", "bad", "10.1.2.3:1234")
		if rr.Code != http.StatusUnauthorized || handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 401/false", rr.Code, handlerRan)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("带凭据的数据响应必须 no-store（否则停用令牌后代理里那份旧 200 还能重放），实得 %q", got)
		}
	})
	t.Run("白名单内 + /api/ + 令牌对 → 放行并把操作人写进 ctx", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/api/oao/thing", "good", "10.1.2.3:1234")
		if rr.Code != http.StatusOK || !handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 200/true", rr.Code, handlerRan)
		}
		if seenOperator != "张三" {
			t.Fatalf("ctx 里的操作人 = %q, want 张三（外部组件的写操作要靠它记审计）", seenOperator)
		}
	})
	t.Run("非 /api/ 路径只过白名单、不看令牌", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/static/oao/mo.css", "", "10.1.2.3:1234")
		if rr.Code != http.StatusOK || !handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 200/true", rr.Code, handlerRan)
		}
	})
}
