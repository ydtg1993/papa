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
	// 这一格以前钉的是"非 /api/ 路径只过白名单、不看令牌"—— 那是**照着当时的实现**写的，
	// 而当时实现里的前缀判断是从 wrap 那里继承来的漏洞：挂在非 /api/ 路径上的业务路由
	// （Router 默认就套着这道 guard）只剩白名单那道门，白名单内的任何人都进得来。
	// 现在两道门对所有路径都生效：白名单是"哪些来源能进"，令牌是"谁"。
	t.Run("非 /api/ 路径：没带令牌 → 401，处理函数不跑", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/internal/report", "", "10.1.2.3:1234")
		if rr.Code != http.StatusUnauthorized || handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 401/false（业务路由不看前缀，令牌是必须的）", rr.Code, handlerRan)
		}
	})
	t.Run("非 /api/ 路径：带对令牌 → 放行并把操作人写进 ctx", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/internal/report", "good", "10.1.2.3:1234")
		if rr.Code != http.StatusOK || !handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 200/true", rr.Code, handlerRan)
		}
		if seenOperator != "张三" {
			t.Fatalf("ctx 里的操作人 = %q, want 张三（业务路由的审计也要能记到人）", seenOperator)
		}
	})
	t.Run("非 /api/ 路径：令牌不对 → 401 且 no-store", func(t *testing.T) {
		rr := call(newMon(), http.MethodGet, "/internal/report", "bad", "10.1.2.3:1234")
		if rr.Code != http.StatusUnauthorized || handlerRan {
			t.Fatalf("status=%d handlerRan=%v, want 401/false", rr.Code, handlerRan)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("带凭据的响应必须 no-store，实得 %q", got)
		}
	})
}

// 反过来钉住**没有**被这次改动碰掉的那条豁免：后台自己的页面（/monitor 的 HTML 壳）
// 只过白名单 —— 令牌是在那张页面里填的，填之前也得拿得到页面。
// 这条用 wrap（内置端点走的那条路），不是 Auth。
func TestWrapKeepsMonitorPageTokenFree(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		Whitelist: []string{"10.0.0.0/8"},
		VerifyToken: func(r *http.Request) (string, bool) {
			return "", false // 一条令牌都不认 —— 页面该照旧打得开
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	m.Register(mux)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/monitor 不该要令牌（令牌在页面里填），status = %d", rr.Code)
	}

	// 同一个客户端打 /api/*：没带令牌必须 401
	req2 := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req2.RemoteAddr = "10.1.2.3:1234"
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("/api/* 必须校验令牌，status = %d", rr2.Code)
	}
}
