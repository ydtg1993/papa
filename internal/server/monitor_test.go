package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/internal/auth"
)

// testLogger 静默日志，仅把 Errorf 转发到测试输出，便于排查。
type testLogger struct{ t *testing.T }

func (l testLogger) Info(args ...any)                  {}
func (l testLogger) Infof(format string, args ...any)  {}
func (l testLogger) Errorf(format string, args ...any) { l.t.Logf(format, args...) }

// fakeStageStats 直接构造各阶段统计快照（纯值假数据，确定性且无并发）。
func fakeStageStats() map[string]crawler.StageStats {
	return map[string]crawler.StageStats{
		"catalog": {
			Global: crawler.GlobalStats{TotalTasks: 8, TotalFailed: 1, TotalTime: 98955000, AvgTime: 12369375, MaxTime: 22137200, MinTime: 6222500},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 3, FailedTasks: 0, TotalTime: 39456100, MaxTime: 19010900, MinTime: 8489000},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 0, TotalTime: 30861700, MaxTime: 15401900, MinTime: 6222500},
				2: {WorkerID: 2, TotalTasks: 2, FailedTasks: 1, TotalTime: 28637200, MaxTime: 22137200, MinTime: 6500000},
			},
			Queue: crawler.QueueStats{Submitted: 8, Completed: 7, Failed: 1, InProgress: 0, QueueLen: 0},
		},
		"detail": {
			Global: crawler.GlobalStats{TotalTasks: 6, TotalFailed: 0, TotalTime: 149893300, AvgTime: 24982216, MaxTime: 40192700, MinTime: 7094600},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 0, TotalTime: 70649100, MaxTime: 40192700, MinTime: 30456400},
				1: {WorkerID: 1, TotalTasks: 4, FailedTasks: 0, TotalTime: 79244200, MaxTime: 33409100, MinTime: 7538800},
			},
			Queue: crawler.QueueStats{Submitted: 6, Completed: 6, Failed: 0, InProgress: 0, QueueLen: 0},
		},
		"video": {
			Global: crawler.GlobalStats{TotalTasks: 10, TotalFailed: 2, TotalTime: 365715800, AvgTime: 36571580, MaxTime: 60246100, MinTime: 10220500},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 1, TotalTime: 106798900, MaxTime: 56096300, MinTime: 50702600},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 1, TotalTime: 71555400, MaxTime: 35336700, MinTime: 15549000},
				2: {WorkerID: 2, TotalTasks: 3, FailedTasks: 0, TotalTime: 112815500, MaxTime: 60246100, MinTime: 10220500},
				3: {WorkerID: 3, TotalTasks: 2, FailedTasks: 0, TotalTime: 74546000, MaxTime: 45605400, MinTime: 28940600},
			},
			Queue: crawler.QueueStats{Submitted: 10, Completed: 8, Failed: 2, InProgress: 0, QueueLen: 0},
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

func emptyGetter() map[string]crawler.StageStats {
	return map[string]crawler.StageStats{}
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

func TestShutdownHandler(t *testing.T) {
	fired := make(chan struct{})
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{OnShutdown: func() { close(fired) }})

	if rr := serve(m, http.MethodPost, "/api/settings/shutdown", nil); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Error("OnShutdown not fired within 1s")
	}
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
