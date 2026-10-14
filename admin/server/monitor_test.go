package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
)

// testLogger 静默日志，仅把 Errorf 转发到测试输出，便于排查。
type testLogger struct{ t *testing.T }

func (l testLogger) Info(args ...any)                  {}
func (l testLogger) Infof(format string, args ...any)  {}
func (l testLogger) Errorf(format string, args ...any) { l.t.Logf(format, args...) }

// fakeStageStats 直接构造各阶段统计快照（纯值假数据，确定性且无并发）。
// 键用 **`site:stage` 的复合键**（与生产一致：阶段名只在本站内唯一，两个站可以各有 catalog），
// 条目里另带裸阶段名与站点 —— 前端按 site 分组、标题用 stage。
func fakeStageStats() map[string]core.StageStats {
	return map[string]core.StageStats{
		"shop:catalog": {
			Stage:  "catalog",
			Site:   "shop",
			Global: core.GlobalStats{TotalTasks: 8, TotalFailed: 1, TotalTime: 98955000, AvgTime: 12369375, MaxTime: 22137200, MinTime: 6222500},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 3, FailedTasks: 0, TotalTime: 39456100, MaxTime: 19010900, MinTime: 8489000},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 0, TotalTime: 30861700, MaxTime: 15401900, MinTime: 6222500},
				2: {WorkerID: 2, TotalTasks: 2, FailedTasks: 1, TotalTime: 28637200, MaxTime: 22137200, MinTime: 6500000},
			},
			Queue: core.QueueStats{Submitted: 8, Completed: 7, Failed: 1, InProgress: 0, QueueLen: 0},
		},
		"shop:detail": {
			Stage:  "detail",
			Site:   "shop",
			Global: core.GlobalStats{TotalTasks: 6, TotalFailed: 0, TotalTime: 149893300, AvgTime: 24982216, MaxTime: 40192700, MinTime: 7094600},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 0, TotalTime: 70649100, MaxTime: 40192700, MinTime: 30456400},
				1: {WorkerID: 1, TotalTasks: 4, FailedTasks: 0, TotalTime: 79244200, MaxTime: 33409100, MinTime: 7538800},
			},
			Queue: core.QueueStats{Submitted: 6, Completed: 6, Failed: 0, InProgress: 0, QueueLen: 0},
		},
		"shop:video": {
			Stage:  "video",
			Site:   "shop",
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

// serveAllowed 与 serve 相同，但把来源 IP 放在白名单里：白名单真的生效的用例要能过那道门
// （httptest 默认的 RemoteAddr 不在任何名单里，直接 serve 会拿到 403）。
func serveAllowed(m *Monitor, method, target string, body io.Reader) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "10.0.0.5:1111"
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
	for _, name := range []string{"shop:catalog", "shop:detail", "shop:video"} {
		st, ok := stages[name].(map[string]any)
		if !ok {
			t.Fatalf("stage %q missing", name)
		}
		for _, key := range []string{"stage", "site", "global", "workers", "queue"} {
			if _, ok := st[key]; !ok {
				t.Errorf("stage %q missing %q", name, key)
			}
		}
		// 标题要用裸阶段名（站点维度由站点 Tab 表达），而不是 `shop:catalog` 这个键
		if got := st["stage"]; got != "catalog" && got != "detail" && got != "video" {
			t.Errorf("stage %q 的 stage 字段应当是裸阶段名，实得 %v", name, got)
		}
		if got := st["site"]; got != "shop" {
			t.Errorf("stage %q 的 site 字段不对：%v", name, got)
		}
	}

	catalog := stages["shop:catalog"].(map[string]any)
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

// whitelistStoreStub 假的配置存储：记下每次保存的条目与操作人，并能注入失败。
// 这些用例全程同步调用（httptest 直接 ServeHTTP），不需要锁。
type whitelistStoreStub struct {
	calls [][]string
	ops   []string
	err   error
}

func (s *whitelistStoreStub) save(entries []string, operator string) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, append([]string(nil), entries...))
	s.ops = append(s.ops, operator)
	return nil
}

func (s *whitelistStoreStub) fail(err error) { s.err = err }

func (s *whitelistStoreStub) saved() [][]string { return s.calls }

func (s *whitelistStoreStub) savedCount() int { return len(s.calls) }

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

// 保存白名单：写时严格（坏条目当场拒收）· 落库成功才换判定缓存 · 落库失败时旧名单继续生效。
//
// 这一版把持久化从"写 whitelist_file"换成了"交给宿主的存储"（papa 落在 crawler_config），
// 所以断言从"文件内容"改成了"存储收到了什么"—— 文件那条路已经不存在了。
func TestWhitelistHandler(t *testing.T) {
	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
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

// 没接配置存储的宿主（admin/server 被直接使用时）：保存返回 404 说明原因，
// 而不是"静默只改内存、重启就没了"。
func TestWhitelistHandlerWithoutStore(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	rr := serve(m, http.MethodPost, "/api/settings/whitelist", strings.NewReader(`{"whitelist":["10.0.0.0/8"]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（%s）", rr.Code, rr.Body.String())
	}
}
func TestSettingsHandler(t *testing.T) {
	dir := t.TempDir()
	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		Whitelist:     []string{"10.0.0.0/8"},
		SaveWhitelist: store.save,
		LogDir:        dir,
	})
	body := decodeJSON(t, serveAllowed(m, http.MethodGet, "/api/settings", nil))

	if got := body["whitelist"].([]any); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("whitelist = %v", body["whitelist"])
	}
	// 前端拿 whitelist_source 拼那句"存哪儿"的提示；原来那对 whitelist_file / has_whitelist_file
	// 已经随白名单搬迁去掉了。
	if got := body["whitelist_source"]; got != whitelistSource {
		t.Errorf("whitelist_source = %v, want %s", got, whitelistSource)
	}
	if got := body["log_dir"]; got != dir {
		t.Errorf("log_dir = %v, want %s", got, dir)
	}
}

// 没接存储的宿主：whitelist_source 为空串 —— 前端据此显示"改动仅本次运行有效"。
func TestSettingsHandlerWithoutStore(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{Whitelist: []string{"10.0.0.0/8"}})
	rr := serveAllowed(m, http.MethodGet, "/api/settings", nil)
	if got := decodeJSON(t, rr)["whitelist_source"]; got != "" {
		t.Fatalf("没接存储时 whitelist_source 应为空串，实得 %v", got)
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

// 暂停/放行的两个后台接口。放行是**人在场的干预**（那一站停了整条抓取线），
// 所以除了状态对不对，还要钉住"操作人确实被传给了宿主去记操作日志"。
//
// 每个站点一把闸门：状态是**列表**（按站点名排序），放行可以点名某个站点，
// 不点名 = 全部放行（后台横幅上那个「恢复抓取」）。站点是必填维度，没有空键那一档。
func TestBreakerEndpoints(t *testing.T) {
	paused := map[string]bool{"sitea": true, "siteb": false} // sitea 被闸住，siteb 正常
	var resumedBy []string
	var stageResumed []string

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
				return map[string]core.BreakerStatus{"sitea": status("sitea"), "siteb": status("siteb")}
			},
			ResumeBreaker: func(site, stage string) bool {
				if !paused[site] {
					return false
				}
				if stage != "" {
					// 阶段级放行：只清那一个阶段那条（这里记一笔就够断言）
					stageResumed = append(stageResumed, site+"/"+stage)
					return true
				}
				paused[site] = false
				return true
			},
			OnBreakerResume: func(operator, site, stage string) {
				resumedBy = append(resumedBy, operator+"@"+site+"/"+stage)
			},
		})
	}
	reset := func() {
		paused = map[string]bool{"sitea": true, "siteb": false}
		resumedBy = nil
		stageResumed = nil
	}

	t.Run("GET 返回各站点的状态，按站点名排序", func(t *testing.T) {
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
		if first["site"] != "sitea" || first["paused"] != true || first["stage"] != "sitea-catalog" {
			t.Fatalf("第一条应当是 sitea：%v", first)
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

	t.Run("不点名 = 全部放行，回调逐站点收到操作人", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["resumed"] != true {
			t.Fatalf("resumed = %v, want true", body["resumed"])
		}
		if scopes, _ := body["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "sitea" {
			t.Fatalf("resumed_scopes = %v，应当只有被闸住的 sitea", body["resumed_scopes"])
		}
		// 测试请求没走鉴权中间件，所以操作人是空串 —— 这里要的是"回调被调了一次、且带上了站点"。
		if len(resumedBy) != 1 || resumedBy[0] != "@sitea/" {
			t.Fatalf("OnBreakerResume 收到 %v，want [@sitea/]（操作人@站点/阶段）", resumedBy)
		}
	})

	t.Run("点名站点只放行那一个", func(t *testing.T) {
		reset()
		paused["siteb"] = true // 两个都被闸住
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=siteb", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if scopes, _ := decodeJSON(t, rr)["resumed_scopes"].([]any); len(scopes) != 1 || scopes[0] != "siteb" {
			t.Fatalf("resumed_scopes = %v，应当只有 siteb", scopes)
		}
		if !paused["sitea"] {
			t.Fatal("sitea 不该被顺手放行")
		}
	})

	t.Run("未知 scope：400 而不是静默放行全部", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume?scope=ghost", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if !paused["sitea"] {
			t.Fatal("报错时不该把 sitea 放行了")
		}
	})

	t.Run("本来就没暂停：resumed=false 而不是报错", func(t *testing.T) {
		reset()
		paused["sitea"] = false
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
	t.Run("点名 stage 放行：交给宿主的也是那个阶段", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/resume", strings.NewReader(`{"scope":"sitea","stage":"catalog"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if body := decodeJSON(t, rr); body["resumed"] != true || body["stage"] != "catalog" {
			t.Fatalf("回执应当说清是哪个阶段：%v", body)
		}
		if len(stageResumed) != 1 || stageResumed[0] != "sitea/catalog" {
			t.Fatalf("应当把 stage 交给宿主：%v", stageResumed)
		}
		if len(resumedBy) != 1 || resumedBy[0] != "@sitea/catalog" {
			t.Fatalf("审计要带上阶段：%v", resumedBy)
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

	t.Run("whitelist 超限：400 且不落存储", func(t *testing.T) {
		store := &whitelistStoreStub{}
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{SaveWhitelist: store.save})
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

	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{SaveWhitelist: store.save})

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
func TestTaskPageEndpoint(t *testing.T) {
	dir := t.TempDir()
	rel := "example/task-7-try-0-abc12345.html"
	const body = "<html><body>archived page</body></html>"
	if err := os.MkdirAll(filepath.Join(dir, "example"), 0o755); err != nil {
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
		for _, bad := range []string{"../secret", "/etc/passwd", "example/../../x", "", "example/.."} {
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
		rr := serve(newMon(dir), http.MethodGet, "/api/task/page?file=example/task-9-try-0-deadbeef.html", nil)
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
		{"example/task-1-try-0-aaaaaaaa.html", filepath.Join(root, "example", "task-1-try-0-aaaaaaaa.html")},
		{"example/./task-1-try-0-aaaaaaaa.html", filepath.Join(root, "example", "task-1-try-0-aaaaaaaa.html")},
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

// 轮询队列的手动触发：全局一条，没有站点参数。
func TestRepeatQueueProcess(t *testing.T) {
	var called int
	newMon := func() *Monitor {
		called = 0
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ProcessRepeatQueue: func() (int, error) { called++; return 3, nil },
		})
	}

	rr := serve(newMon(), http.MethodPost, "/api/repeatqueue/process", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("回调应被调一次，实得 %d", called)
	}
	if body := decodeJSON(t, rr); body["repolled"] != float64(3) {
		t.Fatalf("repolled = %v", body["repolled"])
	}
}

// 错误队列的手动触发与轮询队列同一套：全局一条，没有站点参数。
func TestErrorQueueProcess(t *testing.T) {
	var called int
	newMon := func() *Monitor {
		called = 0
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ProcessErrorQueue: func() (int, error) { called++; return 5, nil },
		})
	}

	rr := serve(newMon(), http.MethodPost, "/api/errorqueue/process", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("回调应被调一次，实得 %d", called)
	}
	if body := decodeJSON(t, rr); body["processed"] != float64(5) {
		t.Fatalf("processed = %v", body["processed"])
	}
}

// 治理队列面板只认两条全局队列，不再有 `<队列>:<站点>` 的站点后缀与 ?site= 参数。
// 前端没有测试框架，这里直接钉静态文件（同 TestMonitorResponseKeysMatchFrontend）。
func TestQueuePanelHasGlobalQueues(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{
		"error_queue: { label: '错误队列'",  // 错误队列进种类表
		"repeat_queue: { label: '轮询队列'", // 轮询队列同样
	} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 里缺少 %q —— 两条治理队列都要在面板种类表里", want)
		}
	}
	// 只钉**旧按站点拆队列**那两个特征串。这里曾经还有一条 `.site === currentSite` ——
	// 它不精确：按站点过滤是**正当**用法（阶段面板、阶段入口卡片都按它分），
	// 当初那条能过只是因为阶段过滤写的是 `(stages[name].site || '') === currentSite`，
	// 中间夹了个 `|| ''` 才没命中。守卫挡不住回归却又会误伤新代码，就不留它。
	for _, bad := range []string{"?site=' + encodeURIComponent", "parseQueue(name)"} {
		if strings.Contains(js, bad) {
			t.Errorf("mo.js 里不该再有按站点拆队列的残留 %q", bad)
		}
	}
}

// /api/monitor 要带上站点快照（后台的站点 Tab 与站点概要靠它）。键是 sites（复数），
// 与 breakers 同一约定；没配回调时不该出现这个键（与 queues/system 一致）。
func TestAPIMonitorCarriesSites(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		SiteStats: func() map[string]core.SiteStat {
			return map[string]core.SiteStat{
				"":      {Key: "", StageCount: 1},
				"siteb": {Key: "siteb", BaseURL: "https://b.example/", StageCount: 2},
			}
		},
	})
	rr := serve(m, http.MethodGet, "/api/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	sites, ok := body["sites"].(map[string]any)
	if !ok || len(sites) != 2 {
		t.Fatalf("sites = %v", body["sites"])
	}
	b, _ := sites["siteb"].(map[string]any)
	if b["base_url"] != "https://b.example/" || b["stage_count"] != float64(2) {
		t.Fatalf("站点快照字段（snake_case）不对：%v", b)
	}

	// 没配 SiteStats 的宿主：这个键应当缺席（前端据此隐藏整排 tab）
	plain := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	if _, ok := decodeJSON(t, serve(plain, http.MethodGet, "/api/monitor", nil))["sites"]; ok {
		t.Fatal("没配 SiteStats 时不该有 sites 键")
	}
}

/* ---------- 阶段入口：点火 + 逐站暂停 ---------- */

func fakeStageEntries() []core.StageEntry {
	return []core.StageEntry{
		{Site: "shop", Stage: "catalog", Mode: "manual", LastTriggeredAt: time.Now().Add(-3 * time.Minute)},
		{Site: "shop", Stage: "detail", Mode: "auto"},
	}
}

// 点火端点：成功要审计（站点/阶段/模式/操作人/IP 都要在）；"点了没用"的那几种原因是
// **409 + 引擎原话**（操作人得看到那句话才知道下一步怎么做），服务器故障才是 500；
// 没配回调一律 404，且 404 与 400 都不记审计（那不是一次触发）。
func TestEntryTriggerEndpoint(t *testing.T) {
	var got []EntryTriggerEvent
	var submitErr error
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			SubmitStageEntries: func(_, _ string) error { return submitErr },
			OnEntryTrigger:     func(ev EntryTriggerEvent) { got = append(got, ev) },
		})
	}
	post := func(body string) *httptest.ResponseRecorder {
		return serve(newMon(), http.MethodPost, "/api/entries/trigger", strings.NewReader(body))
	}

	t.Run("点火成功：回执点名站点与阶段，审计一条", func(t *testing.T) {
		got, submitErr = nil, nil
		rr := post(`{"site":"shop","stage":"catalog"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["site"] != "shop" || body["stage"] != "catalog" {
			t.Fatalf("回执应点名是哪个站哪个阶段：%v", body)
		}
		if len(got) != 1 {
			t.Fatalf("应当记一条审计，实得 %+v", got)
		}
		ev := got[0]
		if ev.Site != "shop" || ev.Stage != "catalog" || ev.Mode != "manual" || ev.Err != nil ||
			ev.At.IsZero() || ev.IP == "" {
			t.Fatalf("审计事件不完整：%+v", ev)
		}
	})

	t.Run("点不动的原因：409 + 原话（含怎么改）", func(t *testing.T) {
		got, submitErr = nil, engine.ErrEntryAutoStage
		rr := post(`{"site":"shop","stage":"detail"}`)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（%s）", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "EntryManual") {
			t.Fatalf("引擎那句话要原样交给操作人：%s", rr.Body.String())
		}
		if len(got) != 1 || got[0].Err == nil {
			t.Fatalf("失败也要记审计（谁在哪一步点了）：%+v", got)
		}
	})

	t.Run("服务器故障才是 500", func(t *testing.T) {
		got, submitErr = nil, errors.New("db down")
		rr := post(`{"site":"shop","stage":"catalog"}`)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rr.Code)
		}
		if len(got) != 1 || got[0].Err == nil {
			t.Fatalf("500 也要记审计：%+v", got)
		}
	})

	t.Run("缺参数 / body 不是 JSON：400 且不记审计", func(t *testing.T) {
		got, submitErr = nil, nil
		for _, body := range []string{`{"site":"shop"}`, `{"stage":"catalog"}`, `{}`, `not json`} {
			if rr := post(body); rr.Code != http.StatusBadRequest {
				t.Fatalf("body=%s status = %d, want 400", body, rr.Code)
			}
		}
		if len(got) != 0 {
			t.Fatalf("400 不该记审计：%+v", got)
		}
	})

	t.Run("query string 也认（给脚本用）", func(t *testing.T) {
		got, submitErr = nil, nil
		rr := serve(newMon(), http.MethodPost, "/api/entries/trigger?site=shop&stage=catalog", nil)
		if rr.Code != http.StatusOK || len(got) != 1 {
			t.Fatalf("status = %d，审计 %d 条", rr.Code, len(got))
		}
	})

	t.Run("没配回调：404 且不记审计", func(t *testing.T) {
		got = nil
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
		rr := serve(m, http.MethodPost, "/api/entries/trigger", strings.NewReader(`{"site":"a","stage":"b"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if len(got) != 0 {
			t.Fatalf("404 不是一次触发，不该记审计：%+v", got)
		}
	})

	t.Run("GET：405", func(t *testing.T) {
		if rr := serve(newMon(), http.MethodGet, "/api/entries/trigger", nil); rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
}

// 暂停端点与 resume 是一对，但**scope 必填**：一个误点不该把全部站点停掉。
func TestBreakerPauseEndpoint(t *testing.T) {
	paused := map[string]bool{"sitea": false, "siteb": false}
	var pausedBy []string
	var stagePaused []string
	newMon := func() *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			BreakerStatuses: func() map[string]core.BreakerStatus {
				return map[string]core.BreakerStatus{"sitea": {Site: "sitea"}, "siteb": {Site: "siteb"}}
			},
			PauseBreaker: func(site, stage string) bool {
				if paused[site] {
					return false
				}
				if stage != "" {
					stagePaused = append(stagePaused, site+"/"+stage)
					return true
				}
				paused[site] = true
				return true
			},
			OnBreakerPause: func(operator, site, stage string) {
				pausedBy = append(pausedBy, operator+"@"+site+"/"+stage)
			},
		})
	}
	reset := func() {
		paused = map[string]bool{"sitea": false, "siteb": false}
		pausedBy = nil
		stagePaused = nil
	}

	t.Run("点名暂停：只停那一站，并记一条审计", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if body := decodeJSON(t, rr); body["paused"] != true {
			t.Fatalf("paused = %v, want true", body["paused"])
		}
		if !paused["sitea"] || paused["siteb"] {
			t.Fatalf("只该停 sitea：%v", paused)
		}
		// 测试请求没走鉴权中间件，操作人是空串 —— 这里要的是"回调被调了一次、且带上了站点"
		if len(pausedBy) != 1 || pausedBy[0] != "@sitea/" {
			t.Fatalf("OnBreakerPause 收到 %v，want [@sitea/]（操作人@站点/阶段）", pausedBy)
		}
	})

	t.Run("本来就在暂停态：paused=false 且不记审计", func(t *testing.T) {
		reset()
		paused["sitea"] = true
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（重复点击不该看起来像失败）", rr.Code)
		}
		if body := decodeJSON(t, rr); body["paused"] != false {
			t.Fatalf("paused = %v, want false", body["paused"])
		}
		if len(pausedBy) != 0 {
			t.Fatalf("没有状态变更就不该记审计：%v", pausedBy)
		}
	})

	t.Run("不点名：400（与 resume 相反：不点名不会停掉全部）", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if paused["sitea"] || paused["siteb"] {
			t.Fatalf("400 时谁都不该被停：%v", paused)
		}
	})

	t.Run("未知站点：400", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"ghost"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("点名 stage：只停那一个阶段，整站状态不动", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"sitea","stage":"catalog"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		body := decodeJSON(t, rr)
		if body["paused"] != true || body["stage"] != "catalog" {
			t.Fatalf("回执应当说清是哪个阶段：%v", body)
		}
		if len(stagePaused) != 1 || stagePaused[0] != "sitea/catalog" {
			t.Fatalf("应当把 stage 交给宿主：%v", stagePaused)
		}
		if paused["sitea"] {
			t.Fatal("点名 stage 时不该把整站也停掉")
		}
		if len(pausedBy) != 1 || pausedBy[0] != "@sitea/catalog" {
			t.Fatalf("审计要带上阶段：%v", pausedBy)
		}
	})

	t.Run("给了 stage 却没给 scope：400（阶段属于某个站）", func(t *testing.T) {
		reset()
		rr := serve(newMon(), http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"stage":"catalog"}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		if len(stagePaused) != 0 {
			t.Fatalf("400 时不该动任何东西：%v", stagePaused)
		}
	})

	t.Run("没配回调：404", func(t *testing.T) {
		reset()
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
		rr := serve(m, http.MethodPost, "/api/breaker/pause", strings.NewReader(`{"scope":"a"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}

// entries 进 /api/monitor：配了回调才有这个键（前端据此隐藏面板），字段名要与前端一致。
func TestAPIMonitorCarriesEntries(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{StageEntries: fakeStageEntries})
	body := decodeJSON(t, serve(m, http.MethodGet, "/api/monitor", nil))
	list, ok := body["entries"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("entries = %v", body["entries"])
	}
	first, _ := list[0].(map[string]any)
	if first["site"] != "shop" || first["stage"] != "catalog" || first["mode"] != "manual" {
		t.Fatalf("第一条不对：%v", first)
	}
	if _, ok := first["last_triggered_at"]; !ok {
		t.Error("要带 last_triggered_at（零值 = 从未点火，前端 fmtAgo 会显示成从未）")
	}

	plain := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	if _, ok := decodeJSON(t, serve(plain, http.MethodGet, "/api/monitor", nil))["entries"]; ok {
		t.Fatal("没配 StageEntries 的宿主：这个键应当缺席（前端据此隐藏整块面板）")
	}
}

// 阶段入口面板的静态接线：渲染函数写进一个**不存在的容器 id** 是静默失效（页面上什么都没有、
// 也没有报错）—— 前端没有测试框架，只能把容器与两个动作钉在静态文件里。
func TestEntryPanelWired(t *testing.T) {
	tpl, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatalf("read template.html: %v", err)
	}
	if !strings.Contains(string(tpl), `id="dash-entries"`) {
		t.Error(`template.html 里没有 id="dash-entries" —— renderEntries 会静默什么都不画`)
	}
	js, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	for _, want := range []string{
		"renderEntries(",
		"triggerEntry(",
		"pauseCrawling(",
		"/api/entries/trigger",
		"/api/breaker/pause",
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("mo.js 里缺少 %q", want)
		}
	}
}

// 前端读的键名必须与服务端写的**一致**。回归点：熔断横幅曾经因为服务端写 "breakers"、
// 前端读 "breaker"，一直拿不到数据、（list || []）把它吞成空数组，一个字都不报 ——
// 多站熔断横幅其实从上线起就没显示过。前端没有测试框架，这里直接钉静态文件里的键名。
func TestMonitorResponseKeysMatchFrontend(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{"data.stages", "data.queues", "data.breakers", "data.sites", "data.entries", "data.custom", "data.system"} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 里没读 %s —— 服务端写了这个键，前端却读别的名字就会静默失效", want)
		}
	}
	// 阶段面板的标题要读条目里的裸阶段名：快照的键是 `site:stage`（阶段名只在本站内唯一），
	// 拿键当标题在多站下会显示成 "shop:catalog"。同 `data.breakers`/`data.breaker` 那条，
	// 前端没有测试框架，只能这样钉住。
	if !strings.Contains(js, "st.stage") {
		t.Error("mo.js 的 stageLabel 没读 st.stage —— 标题会退回 `site:stage` 那个复合键")
	}
	// 横幅判"这次是熔断还是手动"必须读条目里的 `source`。曾经用 `enabled && stage` 猜 ——
	// 阶段级的手动暂停也带 stage（那是它的 scope），在熔断启用着的站上会被说成"熔断"。
	if !strings.Contains(js, "b.source") {
		t.Error("mo.js 的横幅没读 b.source —— 手动的阶段级暂停会被显示成「熔断」")
	}
	// 单数那个是错的（服务端从来不发这个键）
	for _, line := range strings.Split(js, "\n") {
		if strings.Contains(line, "data.breaker ") || strings.Contains(line, "data.breaker)") || strings.Contains(line, "data.breaker;") {
			t.Errorf("mo.js 读了不存在的 data.breaker（服务端发的是 data.breakers）：%s", strings.TrimSpace(line))
		}
	}
}

// 队列「立即执行」也要写审计：成功一条、失败也一条。
// 口径与表格动作 / 令牌页 / 熔断放行一致 —— 那三处都在记，只有这两个端点一直没记，
// 而它们同样会批量改数据（把到点的可轮询任务、失败任务重置并重投）。
func TestQueueProcessWritesAuditEvent(t *testing.T) {
	var events []QueueProcessEvent
	newMon := func() *Monitor {
		events = nil
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ProcessRepeatQueue: func() (int, error) { return 7, nil },
			ProcessErrorQueue:  func() (int, error) { return 5, nil },
			OnQueueProcess:     func(ev QueueProcessEvent) { events = append(events, ev) },
		})
	}

	t.Run("轮询队列成功", func(t *testing.T) {
		serve(newMon(), http.MethodPost, "/api/repeatqueue/process", nil)
		if len(events) != 1 {
			t.Fatalf("审计事件 = %+v，want 恰好一条", events)
		}
		ev := events[0]
		if ev.Queue != engine.QueueRepeat || ev.Processed != 7 || ev.Err != nil {
			t.Fatalf("审计事件 = %+v", ev)
		}
		if ev.IP == "" {
			t.Fatal("来源 IP 应当带上（与白名单同一取法）")
		}
		if ev.At.IsZero() {
			t.Fatal("时间应当带上")
		}
	})

	t.Run("错误队列成功", func(t *testing.T) {
		serve(newMon(), http.MethodPost, "/api/errorqueue/process", nil)
		if len(events) != 1 || events[0].Queue != engine.QueueError || events[0].Processed != 5 {
			t.Fatalf("审计事件 = %+v", events)
		}
	})

	t.Run("处理失败：也要记一条", func(t *testing.T) {
		events = nil
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			ProcessRepeatQueue: func() (int, error) { return 0, fmt.Errorf("db down") },
			OnQueueProcess:     func(ev QueueProcessEvent) { events = append(events, ev) },
		})
		if rr := serve(m, http.MethodPost, "/api/repeatqueue/process", nil); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rr.Code)
		}
		if len(events) != 1 || events[0].Err == nil {
			t.Fatalf("失败的触发也要记一条（err 非空）：%+v", events)
		}
	})

	t.Run("接口没配置（404）：没跑过就不该记", func(t *testing.T) {
		events = nil
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
			OnQueueProcess: func(ev QueueProcessEvent) { events = append(events, ev) },
		})
		if rr := serve(m, http.MethodPost, "/api/repeatqueue/process", nil); rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if len(events) != 0 {
			t.Fatalf("压根没这条接口时不该记审计：%+v", events)
		}
	})
}
