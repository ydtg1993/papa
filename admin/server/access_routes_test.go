package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAuth(t *testing.T) {
	// 令牌校验由宿主注入；这里用桩，既能离线覆盖分支，也验证「操作人进了上下文」
	var gotOperator string
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) {
			switch auth.Extract(r) {
			case "good":
				return "张三", true
			case "disabled":
				return "", false
			default:
				return "", false
			}
		},
	})

	cases := []struct {
		name   string
		target string
		header map[string]string
		want   int
	}{
		{"没有令牌", "/api/monitor", nil, http.StatusUnauthorized},
		{"令牌不对", "/api/monitor", map[string]string{"X-Auth-Key": "nope"}, http.StatusUnauthorized},
		{"令牌被停用", "/api/monitor", map[string]string{"X-Auth-Key": "disabled"}, http.StatusUnauthorized},
		{"bearer", "/api/monitor", map[string]string{"Authorization": "Bearer good"}, http.StatusOK},
		{"bearer 小写方案名（RFC 7235 里同样合法）", "/api/monitor", map[string]string{"Authorization": "bearer good"}, http.StatusOK},
		{"x-auth-key", "/api/monitor", map[string]string{"X-Auth-Key": "good"}, http.StatusOK},
		{"query 不再被接受", "/api/monitor?key=good", nil, http.StatusUnauthorized},
		{"HTML 只查 IP，不要令牌", "/monitor", nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			m.Register(mux)
			mux.Handle("/api/whoami", m.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotOperator = auth.OperatorFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			})))
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Errorf("status = %d, want %d", rr.Code, c.want)
			}
		})
	}

	// 校验通过时，操作人必须进到请求上下文（操作日志靠它记人）
	mux := http.NewServeMux()
	m.Register(mux)
	mux.Handle("/api/whoami", m.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOperator = auth.OperatorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer good")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if gotOperator != "张三" {
		t.Fatalf("上下文里的操作人 = %q, want 张三", gotOperator)
	}
}

func TestAuthNoVerifier(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	rr := serve(m, http.MethodGet, "/api/monitor", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=401", rr.Code)
	}
}

func TestWhitelist(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, Whitelist: []string{"10.0.0.0/8"}})
	mux := http.NewServeMux()
	m.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("whitelisted ip: status = %d, want 200", rr.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req2.RemoteAddr = "203.0.113.9:1234"
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("non-whitelisted ip: status = %d, want 403", rr2.Code)
	}
}

func TestWhitelistHandler(t *testing.T) {
	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		Whitelist:     []string{"10.0.0.0/8"},
		SaveWhitelist: store.save,
	})
	mux := http.NewServeMux()
	m.Register(mux)

	// GET 不允许
	rr := serveAllowed(m, http.MethodGet, "/api/settings/whitelist", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}

	// 不是 IP/CIDR 的条目：400，且**一个字都不写存储**
	rr = serveAllowed(m, http.MethodPost, "/api/settings/whitelist",
		strings.NewReader(`{"whitelist":["10.0.0.0/8","不是IP"]}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("坏条目 status = %d, want 400（%s）", rr.Code, rr.Body.String())
	}
	if got := store.savedCount(); got != 0 {
		t.Fatalf("坏条目不该写存储，实得写了 %d 次", got)
	}

	// 正常保存：落库 + 立刻生效
	body := `{"whitelist":["10.0.0.0/8","192.168.1.1"]}`
	rr2 := serveAllowed(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(body))
	if rr2.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", rr2.Code, rr2.Body.String())
	}
	if saved := store.saved(); len(saved) != 1 || len(saved[0]) != 2 {
		t.Fatalf("存储应收到 2 条，实得 %v", saved)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "10.0.0.5:1111"
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req)
	if rr3.Code != http.StatusOK {
		t.Errorf("after update whitelisted ip status = %d, want 200", rr3.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "8.8.8.8:2222"
	rr4 := httptest.NewRecorder()
	mux.ServeHTTP(rr4, req)
	if rr4.Code != http.StatusForbidden {
		t.Errorf("after update non-whitelisted ip status = %d, want 403", rr4.Code)
	}

	// 存储写失败 → 500，且**旧名单继续生效**：不能出现"本进程放行、重启后不认"的白名单
	store.fail(errors.New("库抖了"))
	rr5 := serveAllowed(m, http.MethodPost, "/api/settings/whitelist",
		strings.NewReader(`{"whitelist":["192.168.0.0/16"]}`))
	if rr5.Code != http.StatusInternalServerError {
		t.Fatalf("存储失败 status = %d, want 500（%s）", rr5.Code, rr5.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "10.0.0.5:1111"
	rr6 := httptest.NewRecorder()
	mux.ServeHTTP(rr6, req)
	if rr6.Code != http.StatusOK {
		t.Errorf("保存失败后旧名单该继续生效，status = %d, want 200", rr6.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "192.168.0.5:1111"
	rr7 := httptest.NewRecorder()
	mux.ServeHTTP(rr7, req)
	if rr7.Code != http.StatusForbidden {
		t.Errorf("保存失败时新名单不该生效，status = %d, want 403", rr7.Code)
	}
}

func TestWhitelistHandlerWithoutStore(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests})
	rr := serve(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(`{"whitelist":["10.0.0.0/8"]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（%s）", rr.Code, rr.Body.String())
	}
}

func TestAdminJSONHandlersRejectOversizedBody(t *testing.T) {
	oversized := func(key string) string {
		return `{"` + key + `":["` + strings.Repeat("a", (64<<10)) + `"]}`
	}

	t.Run("whitelist 超限：400 且不落存储", func(t *testing.T) {
		store := &whitelistStoreStub{}
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, SaveWhitelist: store.save})
		rr := serve(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(oversized("whitelist")))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body %d 字节）", rr.Code, rr.Body.Len())
		}
		if got := store.savedCount(); got != 0 {
			t.Fatalf("超限的请求不该写存储，实得 %d 次", got)
		}

		// 关键：解析失败不能把白名单改成半截 —— 生效的那份必须还是"没设过"（空 → 不限制），
		// 而不是那串被截断的 aaaa…
		body := decodeJSON(t, serve(m, http.MethodGet, "/api/settings", nil))
		if got := body["whitelist"].([]any); len(got) != 0 {
			t.Fatalf("超限的请求不该改动生效的白名单，实得 %v", got)
		}
	})

}

func TestWhitelistHandlerAcceptsLargeButBoundedBody(t *testing.T) {
	const entries = 1000
	list := make([]string, 0, entries)
	for i := 0; i < entries; i++ {
		list = append(list, "10.0.1."+strconv.Itoa(i%256))
	}
	body, err := json.Marshal(map[string]any{"whitelist": list})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= (64 << 10) {
		t.Fatalf("前置条件：这份 body 应当在上限之内（%d >= %d）", len(body), (64 << 10))
	}

	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, SaveWhitelist: store.save})

	rr := serve(m, http.MethodPost, "/api/settings/whitelist", bytes.NewReader(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body %d 字节）", rr.Code, rr.Body.Len())
	}

	saved := store.saved()
	if len(saved) != 1 || len(saved[0]) != entries {
		t.Fatalf("存储应收到 %d 条，实得 %v 条", entries, len(saved[0]))
	}
	// 而且真的生效了：名单里的第一条放行，名单外的一律 403
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "10.0.1.0:1111"
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("名单内的来源该放行，status = %d", rr2.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.RemoteAddr = "203.0.113.9:1111"
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req)
	if rr3.Code != http.StatusForbidden {
		t.Fatalf("名单外的来源该拦，status = %d", rr3.Code)
	}
}

func TestWhitelistHandlerRejectsMissingOrNull(t *testing.T) {
	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		Whitelist:     []string{"10.0.0.0/8"},
		SaveWhitelist: store.save,
	})

	for _, body := range []string{`{}`, `{"whitelist":null}`} {
		rr := serveAllowed(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(body))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status = %d, want 400（%s）", body, rr.Code, rr.Body.String())
		}
	}
	if got := store.savedCount(); got != 0 {
		t.Fatalf("缺失/null 不该写存储，实得写了 %d 次", got)
	}

	// 显式 [] 才是清空
	rr := serveAllowed(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(`{"whitelist":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("显式 [] status = %d, want 200（%s）", rr.Code, rr.Body.String())
	}
	if saved := store.saved(); len(saved) != 1 || len(saved[0]) != 0 {
		t.Fatalf("显式 [] 应清空存储，实得 %v", saved)
	}
}

func TestMonitorTemplateAuthRequiredFlag(t *testing.T) {
	verify := func(*http.Request) (string, bool) { return "", true }
	for _, tt := range []struct {
		name string
		cfg  MonitorConfig
		want string
	}{
		{"未注入校验器仍要求登录", MonitorConfig{}, `data-auth-required="1"`},
		{"宿主显式定制登录界面（不影响接口认证）", MonitorConfig{VerifyToken: verify, AuthRequired: func() bool { return false }}, `data-auth-required="0"`},
		{"有令牌", MonitorConfig{VerifyToken: verify, AuthRequired: func() bool { return true }}, `data-auth-required="1"`},
		{"只给校验器时保守要求登录", MonitorConfig{VerifyToken: verify}, `data-auth-required="1"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := serve(NewMonitor(emptyGetter, testLogger{t}, tt.cfg), http.MethodGet, "/monitor", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("页面里缺少 %q", tt.want)
			}
		})
	}
}

func TestMoJSLoginGateFollowsServerAuthMode(t *testing.T) {
	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	raw := string(js)
	for _, want := range []string{
		"function authRequired()",
		"if (deps.authRequired() && !deps.loadKey())",
		"startPolling();",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("mo.js 里缺少 %q", want)
		}
	}
	requests, err := fs.ReadFile(staticFS, "static/requests.js")
	if err != nil || !strings.Contains(string(requests), "function startPolling()") {
		t.Fatalf("独立轮询模块缺失: %v", err)
	}
	if strings.Contains(raw, "if (loadKey()) hideLogin(); else showLogin(false);") {
		t.Error("mo.js 忽略宿主的登录界面定制")
	}
}
