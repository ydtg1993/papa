package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
)

// testLogger 静默日志，仅把 Errorf 转发到测试输出，便于排查。
type testLogger struct{ t *testing.T }

func (l testLogger) Info(args ...any)                  {}
func (l testLogger) Infof(format string, args ...any)  {}
func (l testLogger) Errorf(format string, args ...any) { l.t.Logf(format, args...) }

// fakeStageStats 直接构造各阶段统计快照（纯值假数据，确定性且无并发）。
func fakeStageStats() map[string]core.StageStats {
	return map[string]core.StageStats{
		"catalog": {
			Global: core.GlobalStats{TotalTasks: 8, TotalFailed: 1, TotalTime: 98955000, AvgTime: 12369375, MaxTime: 22137200, MinTime: 6222500},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 3, FailedTasks: 0, TotalTime: 39456100, MaxTime: 19010900, MinTime: 8489000},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 0, TotalTime: 30861700, MaxTime: 15401900, MinTime: 6222500},
				2: {WorkerID: 2, TotalTasks: 2, FailedTasks: 1, TotalTime: 28637200, MaxTime: 22137200, MinTime: 6500000},
			},
			Queue: core.QueueStats{Submitted: 8, Completed: 7, Failed: 1, InProgress: 0, QueueLen: 0},
		},
		"detail": {
			Global: core.GlobalStats{TotalTasks: 6, TotalFailed: 0, TotalTime: 149893300, AvgTime: 24982216, MaxTime: 40192700, MinTime: 7094600},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 0, TotalTime: 70649100, MaxTime: 40192700, MinTime: 30456400},
				1: {WorkerID: 1, TotalTasks: 4, FailedTasks: 0, TotalTime: 79244200, MaxTime: 33409100, MinTime: 7538800},
			},
			Queue: core.QueueStats{Submitted: 6, Completed: 6, Failed: 0, InProgress: 0, QueueLen: 0},
		},
		"video": {
			Global: core.GlobalStats{TotalTasks: 10, TotalFailed: 2, TotalTime: 365715800, AvgTime: 36571580, MaxTime: 60246100, MinTime: 10220500},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 1, TotalTime: 106798900, MaxTime: 56096300, MinTime: 50702600},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 1, TotalTime: 71555400, MaxTime: 35336700, MinTime: 15549000},
				2: {WorkerID: 2, TotalTasks: 3, FailedTasks: 0, TotalTime: 112815500, MaxTime: 60246100, MinTime: 10220500},
				3: {WorkerID: 3, TotalTasks: 2, FailedTasks: 0, TotalTime: 74546000, MaxTime: 45605400, MinTime: 28940600},
			},
			Queue: core.QueueStats{Submitted: 10, Completed: 8, Failed: 2, InProgress: 0, QueueLen: 0},
		},
	}
}

// fakeMetricsSnapshot 造一批业务自定义指标快照。
func fakeMetricsSnapshot() map[string]any {
	return map[string]any{
		"catalog_total": 128,
		"detail_total":  96,
		"video_total":   42,
		"success_rate":  0.972,
		"running":       true,
		"started_at":    "2026-09-29T08:00:00Z",
	}
}

// newFakeMonitor 组装一个带全套假数据的监控路由。
func newFakeMonitor(t *testing.T) *Monitor {
	t.Helper()

	return NewMonitor(
		fakeStageStats,
		testLogger{t: t},
		MonitorConfig{
			Metrics: fakeMetricsSnapshot,
		},
	)
}

// serve 注册监控路由并发起一次请求。
func serve(m *Monitor, method, target string, body io.Reader) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(method, target, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode json: %v (body=%s)", err, rr.Body.String())
	}
	return v
}

func emptyGetter() map[string]core.StageStats {
	return map[string]core.StageStats{}
}

func TestAPIMonitor(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/api/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)

	stages, ok := body["stages"].(map[string]any)
	if !ok {
		t.Fatalf("stages missing or wrong type: %T", body["stages"])
	}
	for _, name := range []string{"catalog", "detail", "video"} {
		st, ok := stages[name].(map[string]any)
		if !ok {
			t.Fatalf("stage %q missing", name)
		}
		for _, key := range []string{"global", "workers", "queue"} {
			if _, ok := st[key]; !ok {
				t.Errorf("stage %q missing %q", name, key)
			}
		}
	}

	catalog := stages["catalog"].(map[string]any)
	queue := catalog["queue"].(map[string]any)
	if got := queue["submitted"]; got != float64(8) {
		t.Errorf("catalog submitted = %v, want 8", got)
	}
	if got := queue["completed"]; got != float64(7) {
		t.Errorf("catalog completed = %v, want 7", got)
	}
	if got := queue["failed"]; got != float64(1) {
		t.Errorf("catalog failed = %v, want 1", got)
	}

	global := catalog["global"].(map[string]any)
	if got := global["TotalTasks"]; got != float64(8) {
		t.Errorf("catalog TotalTasks = %v, want 8", got)
	}
	if got := global["TotalFailed"]; got != float64(1) {
		t.Errorf("catalog TotalFailed = %v, want 1", got)
	}

	if workers := catalog["workers"].(map[string]any); len(workers) == 0 {
		t.Error("catalog workers should be non-empty")
	}

	custom := body["custom"].(map[string]any)
	if got := custom["catalog_total"]; got != float64(128) {
		t.Errorf("custom catalog_total = %v, want 128", got)
	}
	if _, ok := body["system"]; ok {
		t.Error("system should be absent when SysInfo is nil")
	}
}

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
			mux.HandleFunc("/api/whoami", m.wrap(func(w http.ResponseWriter, r *http.Request) {
				gotOperator = auth.OperatorFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
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
	mux.HandleFunc("/api/whoami", m.wrap(func(w http.ResponseWriter, r *http.Request) {
		gotOperator = auth.OperatorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer good")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if gotOperator != "张三" {
		t.Fatalf("上下文里的操作人 = %q, want 张三", gotOperator)
	}
}

// 带凭据的接口响应不许被缓存：令牌停用/删除后，代理里那份旧 200 还能被重放出来。
// （浏览器自己不会缓存带 Authorization 的响应，但中间代理会 —— 所以显式写死，不靠实现细节。）
func TestAPIResponsesNotCached(t *testing.T) {
	ok := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) { return "张三", true },
	})
	deny := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) { return "", false },
	})

	for _, m := range []*Monitor{ok, deny} {
		mux := http.NewServeMux()
		m.Register(mux)
		for _, target := range []string{"/api/monitor", "/api/settings", "/api/logs"} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
			if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s（%d）的 Cache-Control = %q，want no-store", target, rr.Code, cc)
			}
		}
	}
}

// 没注入校验器（未配置凭据）时 /api/* 放行 —— 与"没配任何凭据"同义
func TestAuthNoVerifier(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	mux := http.NewServeMux()
	m.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/monitor", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestWhitelist(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{Whitelist: []string{"10.0.0.0/8"}})
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
	dir := t.TempDir()
	wf := filepath.Join(dir, "whitelist.txt")

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{WhitelistFile: wf})
	mux := http.NewServeMux()
	m.Register(mux)

	// GET 不允许
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/settings/whitelist", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}

	// POST 更新白名单并持久化
	body := `{"whitelist":["10.0.0.0/8","192.168.1.1"]}`
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/api/settings/whitelist", strings.NewReader(body)))
	if rr2.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", rr2.Code, rr2.Body.String())
	}
	if b, err := os.ReadFile(wf); err != nil {
		t.Fatalf("read whitelist file: %v", err)
	} else if !strings.Contains(string(b), "10.0.0.0/8") {
		t.Errorf("whitelist file content = %q, want contains 10.0.0.0/8", string(b))
	}

	// 更新后：新白名单内的 IP 放行，其余拒绝
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
}

func TestSettingsHandler(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "w.txt")
	if err := os.WriteFile(wf, []byte("10.0.0.0/8\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		Whitelist:     []string{"10.0.0.0/8"},
		WhitelistFile: wf,
		LogDir:        dir,
	})
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.RemoteAddr = "10.0.0.5:1111"
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := decodeJSON(t, rr)

	if got := body["whitelist"].([]any); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("whitelist = %v", body["whitelist"])
	}
	if got := body["has_whitelist_file"]; got != true {
		t.Errorf("has_whitelist_file = %v, want true", got)
	}
	if got := body["log_dir"]; got != dir {
		t.Errorf("log_dir = %v, want %s", got, dir)
	}
}

func TestLogsListHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.log"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{LogDir: dir})
	rr := serve(m, http.MethodGet, "/api/logs", nil)
	body := decodeJSON(t, rr)

	files := body["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files count = %d, want 2", len(files))
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"a.log", "b.log"} {
		if !names[n] {
			t.Errorf("missing log file %q", n)
		}
	}
}

func TestLogsDownloadHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{LogDir: dir})

	rr := serve(m, http.MethodGet, "/api/logs/download?file=a.log", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("single download status = %d", rr.Code)
	}
	if rr.Body.String() != "hello" {
		t.Errorf("single download body = %q, want %q", rr.Body.String(), "hello")
	}

	rr2 := serve(m, http.MethodGet, "/api/logs/download", nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("zip download status = %d", rr2.Code)
	}
	if ct := rr2.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip content-type = %q, want application/zip", ct)
	}
}

func TestHTMLHandler(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// 关停是高危操作：body 里必须带一个校验得过的令牌，否则不触发。
func TestShutdownHandlerRequiresToken(t *testing.T) {
	newMon := func(fired chan struct{}) *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			OnShutdown: func() { close(fired) },
			VerifyTokenValue: func(token string) (string, bool) {
				if token == "good" {
					return "张三", true
				}
				return "", false
			},
		})
	}
	// 没触发就说明被拦下了 —— 给足一点时间，别把"慢"误判成"被拦"
	assertNotFired := func(t *testing.T, fired chan struct{}, what string) {
		t.Helper()
		select {
		case <-fired:
			t.Fatalf("%s：不该触发关停", what)
		case <-time.After(150 * time.Millisecond):
		}
	}

	t.Run("令牌不对 → 403 且不关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"bad"}`))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		assertNotFired(t, fired, "令牌不对")
	})

	t.Run("没带令牌 → 403 且不关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		if rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{}`)); rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		assertNotFired(t, fired, "没带令牌")
	})

	t.Run("请求体不是 JSON → 400", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		if rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader("not json")); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		assertNotFired(t, fired, "请求体非法")
	})

	t.Run("令牌正确 → 关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"good"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		select {
		case <-fired:
		case <-time.After(time.Second):
			t.Error("OnShutdown 没在 1s 内触发")
		}
	})

	t.Run("没配校验器 → 不支持关停（fail closed）", func(t *testing.T) {
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{OnShutdown: func() {}})
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"good"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}

func TestParseWhitelist(t *testing.T) {
	got := parseWhitelist([]string{"10.0.0.1", "192.168.0.0/16", "::1", "bad", ""}, testLogger{t})
	if len(got) != 3 {
		t.Fatalf("parsed count = %d, want 3", len(got))
	}
	if got[0].String() != "10.0.0.1/32" {
		t.Errorf("single ip = %s, want 10.0.0.1/32", got[0])
	}
	if got[1].String() != "192.168.0.0/16" {
		t.Errorf("cidr = %s, want 192.168.0.0/16", got[1])
	}
	if got[2].String() != "::1/128" {
		t.Errorf("ipv6 = %s, want ::1/128", got[2])
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	if ip := clientIP(req); !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("clientIP = %v, want 1.2.3.4", ip)
	}
	req.RemoteAddr = "1.2.3.4"
	if ip := clientIP(req); !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("clientIP(no port) = %v, want 1.2.3.4", ip)
	}
}

func TestMonitorDemoJSON(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/api/monitor", nil)

	var v any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join("testdata", "monitor_demo.json")
	if err := os.WriteFile(out, pretty, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("demo json written to %s", out)
	t.Logf("\n%s", pretty)
}

// 步骤追踪接口：非法 id 挡在解析层、未配置返回 404、业务错误（如追踪开关没开）
// 原样带给前端而不是被吞成 500 —— 抽屉里要能显示那句「步骤追踪未开启」。
func TestTaskTraceHandler(t *testing.T) {
	steps := []core.TraceStep{
		{Attempt: 0, Seq: 0, Step: "打开列表页", Status: "ok", Duration: 12 * time.Millisecond},
		{Attempt: 0, Seq: 1, Step: "解析详情", Status: "failed", Kind: "no-retry", Message: "selector not found"},
	}
	withTrace := NewMonitor(fakeStageStats, testLogger{t: t}, MonitorConfig{
		TaskTrace: func(id int) ([]core.TraceStep, error) {
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
		m := NewMonitor(fakeStageStats, testLogger{t: t}, MonitorConfig{
			TaskTrace: func(int) ([]core.TraceStep, error) {
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

// 熔断闸门的两个后台接口。放行是**人在场的干预**（熔断停了整条抓取线），
// 所以除了状态对不对，还要钉住"操作人确实被传给了宿主去记操作日志"。
//
// 多站之后每个 scope 一把闸门：状态是**列表**（默认 scope 排第一），放行可以点名某个 scope，
// 不点名 = 全部放行（后台横幅上那个「恢复抓取」）。
func TestBreakerEndpoints(t *testing.T) {
	paused := map[string]bool{"": true, "siteb": false} // 默认 scope 被闸住，siteb 正常
	var resumedBy []string

	status := func(site string) core.BreakerStatus {
		return core.BreakerStatus{
			Enabled: true, Site: site, Paused: paused[site], PausedAt: time.Now(),
			Reason: "窗口内终态失败数达到阈值", Stage: site + "-catalog",
			Failures: 50, Threshold: 50, Window: 5 * time.Minute, InWindow: 12,
		}
	}
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			BreakerStatuses: func() map[string]core.BreakerStatus {
				return map[string]core.BreakerStatus{"": status(""), "siteb": status("siteb")}
			},
			ResumeBreaker: func(site string) bool {
				if !paused[site] {
					return false
				}
				paused[site] = false
				return true
			},
			OnBreakerResume: func(operator, site string) {
				resumedBy = append(resumedBy, operator+"@"+site)
			},
		})
	}
	reset := func() {
		paused = map[string]bool{"": true, "siteb": false}
		resumedBy = nil
	}

	t.Run("GET 返回各 scope 的状态，默认 scope 排第一", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodGet, "/api/breaker", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		list, ok := body["breakers"].([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("breakers = %v", body["breakers"])
		}
		first, _ := list[0].(map[string]any)
		if first["site"] != "" || first["paused"] != true || first["stage"] != "-catalog" {
			t.Fatalf("第一条应当是默认 scope：%v", first)
		}
		if first["in_window"] != float64(12) {
			t.Fatalf("in_window = %v, want 12（实时值，和触发快照的 failures 不是一回事）", first["in_window"])
		}
		second, _ := list[1].(map[string]any)
		if second["site"] != "siteb" || second["paused"] != false {
			t.Fatalf("第二条应当是 siteb：%v", second)
		}
	})

	t.Run("状态也随 /api/monitor 一起返回", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodGet, "/api/monitor", nil)
		body := decodeJSON(t, rr)
		list, ok := body["breakers"].([]any)
		if !ok || len(list) != 2 {
			t.Fatalf("monitor 响应里应带 breakers 数组，实得 %v", body["breakers"])
		}
	})

	t.Run("不点名 = 全部放行，回调逐 scope 收到操作人", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["resumed"] != true {
			t.Fatalf("resumed = %v, want true", body["resumed"])
		}
		if scopes, _ := body["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "" {
			t.Fatalf("resumed_scopes = %v，应当只有被闸住的默认 scope", body["resumed_scopes"])
		}
		// 测试请求没走鉴权中间件，所以操作人是空串 —— 这里要的是"回调被调了一次、且带上了 scope"。
		if len(resumedBy) != 1 || resumedBy[0] != "@" {
			t.Fatalf("OnBreakerResume 收到 %v，want [@]（操作人@scope）", resumedBy)
		}
	})

	t.Run("点名 scope 只放行那一个", func(t *testing.T) {
		reset()
		paused["siteb"] = true // 两个都被闸住
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=siteb", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if scopes, _ := decodeJSON(t, rr)["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "siteb" {
			t.Fatalf("resumed_scopes = %v，应当只有 siteb", scopes)
		}
		if !paused[""] {
			t.Fatal("默认 scope 不该被顺手放行")
		}
	})

	t.Run("未知 scope：400 而不是静默放行全部", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=ghost", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if !paused[""] {
			t.Fatal("报错时不该把默认 scope 放行了")
		}
	})

	t.Run("本来就没暂停：resumed=false 而不是报错", func(t *testing.T) {
		reset()
		paused[""] = false
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（重复点击恢复不该看起来像失败）", rr.Code)
		}
		body := decodeJSON(t, rr)
		if body["resumed"] != false {
			t.Fatalf("resumed = %v, want false", body["resumed"])
		}
		if len(resumedBy) != 0 {
			t.Fatal("没真的放行就不该记操作日志")
		}
	})

	t.Run("GET 不接受写方法", func(t *testing.T) {
		if rr := serve(newMon(), http.MethodPost, "/api/breaker", nil); rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
}

// 后台收 JSON 的那两个接口必须给请求体设上限。
//
// 回归点：`json.NewDecoder(r.Body)` 会把一个没写完的 JSON（比如永远不闭合的数组）
// 一路读下去 —— 不设限就是任人喂内存。这条路径在鉴权后面，但"要令牌"不等于"不用管"。
//
// 这里故意用**合法的、只是超长**的 JSON：证明拦住它的是上限，不是"解析不了"。
func TestAdminJSONHandlersRejectOversizedBody(t *testing.T) {
	oversized := func(key string) string {
		return `{"` + key + `":["` + strings.Repeat("a", maxAdminBody) + `"]}`
	}

	t.Run("whitelist 超限：400 且不落盘", func(t *testing.T) {
		dir := t.TempDir()
		wf := filepath.Join(dir, "whitelist.txt")
		original := "10.0.0.0/8\n"
		if err := os.WriteFile(wf, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}

		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{WhitelistFile: wf})
		rr := serve(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(oversized("whitelist")))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body %d 字节）", rr.Code, rr.Body.Len())
		}

		// 关键：解析失败不能把白名单改成半截 —— 文件必须还是原样
		got, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original {
			t.Fatalf("超限的请求不该改动白名单文件：%q", got)
		}
	})

	t.Run("config 超限：400 且不下发", func(t *testing.T) {
		applied := 0
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ConfigGet: func() *config.RuntimeConfig { return &config.RuntimeConfig{} },
			ConfigSet: func(*config.RuntimeConfig) error {
				applied++
				return nil
			},
		})

		rr := serve(m, http.MethodPut, "/api/config", strings.NewReader(oversized("html")))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（body %d 字节）", rr.Code, rr.Body.Len())
		}
		if applied != 0 {
			t.Fatalf("超限时不该调用 ConfigSet，实调 %d 次", applied)
		}
	})
}

// 反过来钉住上限没有小到误伤：一份接近上限、但仍能装下的白名单要照常生效。
// （64KB 约合 3000 条 IP/CIDR，这里放 1000 条。）
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
	if len(body) >= maxAdminBody {
		t.Fatalf("前置条件：这份 body 应当在上限之内（%d >= %d）", len(body), maxAdminBody)
	}

	dir := t.TempDir()
	wf := filepath.Join(dir, "whitelist.txt")
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{WhitelistFile: wf})

	rr := serve(m, http.MethodPost, "/api/settings/whitelist", bytes.NewReader(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body %d 字节）", rr.Code, rr.Body.Len())
	}

	got, err := os.ReadFile(wf)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimRight(string(got), "\n"), "\n") + 1
	if lines != entries {
		t.Fatalf("落盘条数 = %d, want %d", lines, entries)
	}
}

// 归档页面的下载接口：给的就是文件本身（拿去本地分析），路径是**不可信输入**，越界一律拒。
func TestTaskPageEndpoint(t *testing.T) {
	dir := t.TempDir()
	rel := "huangguo/task-7-try-0-abc12345.html"
	const body = "<html><body>archived page</body></html>"
	if err := os.MkdirAll(filepath.Join(dir, "huangguo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	newMon := func(archiveDir string) *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ArchiveDir: archiveDir})
	}

	t.Run("正常下载：附件形式、内容原样", func(t *testing.T) {
		rr := serve(newMon(dir), http.MethodGet, "/api/task/page?file="+url.QueryEscape(rel), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if rr.Body.String() != body {
			t.Fatalf("body = %q", rr.Body.String())
		}
		if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Fatalf("应当是附件下载，实得 %q", cd)
		}
	})

	t.Run("路径越界一律拒", func(t *testing.T) {
		for _, bad := range []string{"../secret", "/etc/passwd", "huangguo/../../x", "", "huangguo/.."} {
			rr := serve(newMon(dir), http.MethodGet, "/api/task/page?file="+url.QueryEscape(bad), nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("file=%q 应当 400，实得 %d", bad, rr.Code)
			}
		}
	})

	t.Run("归档没开：404", func(t *testing.T) {
		rr := serve(newMon(""), http.MethodGet, "/api/task/page?file="+url.QueryEscape(rel), nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("文件不在（过期被清了）：404", func(t *testing.T) {
		rr := serve(newMon(dir), http.MethodGet, "/api/task/page?file=huangguo/task-9-try-0-deadbeef.html", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("不接受写方法", func(t *testing.T) {
		if rr := serve(newMon(dir), http.MethodPost, "/api/task/page?file="+url.QueryEscape(rel), nil); rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
}

// resolveArchivedFile 的两道判据：路径语义清洗 + 解析后必须仍在根下。
func TestResolveArchivedFile(t *testing.T) {
	root := filepath.Join("tmp", "archive")
	ok := []struct{ rel, want string }{
		{"huangguo/task-1-try-0-aaaaaaaa.html", filepath.Join(root, "huangguo", "task-1-try-0-aaaaaaaa.html")},
		{"huangguo/./task-1-try-0-aaaaaaaa.html", filepath.Join(root, "huangguo", "task-1-try-0-aaaaaaaa.html")},
	}
	for _, c := range ok {
		got, err := resolveArchivedFile(root, c.rel)
		if err != nil || got != c.want {
			t.Fatalf("resolve(%q) = %q, %v; want %q", c.rel, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/../../x", "/abs/path", "a\x00b"} {
		if _, err := resolveArchivedFile(root, bad); err == nil {
			t.Fatalf("resolve(%q) 应当报错", bad)
		}
	}
}
