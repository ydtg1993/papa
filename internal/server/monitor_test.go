package server

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/ydtg1993/papa/v2/internal/dataadmin"
	"github.com/ydtg1993/papa/v2/pkg/metrics"
	"github.com/ydtg1993/papa/v2/pkg/track"
	"github.com/ydtg1993/papa/v2/pkg/workerpool"
)

// testLogger 静默日志，仅把 Errorf 转发到测试输出，便于排查。
type testLogger struct{ t *testing.T }

func (l testLogger) Info(args ...any)                  {}
func (l testLogger) Infof(format string, args ...any)  {}
func (l testLogger) Errorf(format string, args ...any) { l.t.Logf(format, args...) }

// fakeTask 描述一条假任务：id 唯一、fail 是否失败、duration 处理耗时。
type fakeTask struct {
	id       int
	url      string
	fail     bool
	duration time.Duration
}

// newFakeStage 用真实 WorkerPool + StatsQueue 跑一批假任务，产出真实的队列/worker/全局统计。
func newFakeStage(t *testing.T, stage string, workers int, tasks []fakeTask) *track.StatsQueue[*crawler.Task] {
	t.Helper()

	pool := workerpool.NewWorkerPool[*crawler.Task](workers, 256)
	stats := track.NewStatsQueue(pool)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stats.Start(ctx)

	failIDs := make(map[int]bool, len(tasks))
	durByID := make(map[int]time.Duration, len(tasks))
	for _, tk := range tasks {
		failIDs[tk.id] = tk.fail
		durByID[tk.id] = tk.duration
	}

	pool.Start(ctx, func(ctx context.Context, task *crawler.Task) error {
		if d := durByID[task.ID]; d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
		if failIDs[task.ID] {
			return errors.New("simulated failure")
		}
		return nil
	})

	for _, tk := range tasks {
		if err := pool.Submit(&crawler.Task{ID: tk.id, Stage: stage, URL: tk.url}); err != nil {
			t.Fatalf("submit %s/%d: %v", stage, tk.id, err)
		}
	}
	pool.Stop(5 * time.Second)

	// 等监控消费者把 activity 通道排空，避免异步读竞态。
	want := int64(len(tasks))
	deadline := time.Now().Add(3 * time.Second)
	for stats.GetGlobalStats().TotalTasks != want {
		if time.Now().After(deadline) {
			t.Fatalf("stage %s: stats not drained: got %d want %d", stage, stats.GetGlobalStats().TotalTasks, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return stats
}

type fakeUser struct {
	ID   uint `gorm:"primarykey"`
	Name string
	Age  int
	VIP  bool
	At   time.Time
}

type fakeOrder struct {
	ID     uint `gorm:"primarykey"`
	UserID uint
	Amount float64
	Status string
}

// fakeMetrics 造一批业务自定义指标。
func fakeMetrics() *metrics.Registry {
	r := metrics.New()
	r.Set("catalog_total", 128)
	r.Set("detail_total", 96)
	r.Set("video_total", 42)
	r.Set("success_rate", 0.972)
	r.Set("running", true)
	r.Set("started_at", "2026-09-29T08:00:00Z")
	return r
}

// newFakeMonitor 组装一个带全套假数据的监控路由。
func newFakeMonitor(t *testing.T) *Monitor {
	t.Helper()

	stages := map[string]*track.StatsQueue[*crawler.Task]{
		"catalog": newFakeStage(t, "catalog", 3, []fakeTask{
			{id: 1, url: "https://example.com/catalog/1", duration: 8 * time.Millisecond},
			{id: 2, url: "https://example.com/catalog/2", duration: 15 * time.Millisecond},
			{id: 3, url: "https://example.com/catalog/3", duration: 5 * time.Millisecond},
			{id: 4, url: "https://example.com/catalog/4", duration: 22 * time.Millisecond, fail: true},
			{id: 5, url: "https://example.com/catalog/5", duration: 11 * time.Millisecond},
			{id: 6, url: "https://example.com/catalog/6", duration: 9 * time.Millisecond},
			{id: 7, url: "https://example.com/catalog/7", duration: 18 * time.Millisecond},
			{id: 8, url: "https://example.com/catalog/8", duration: 6 * time.Millisecond},
		}),
		"detail": newFakeStage(t, "detail", 2, []fakeTask{
			{id: 1, url: "https://example.com/detail/1", duration: 30 * time.Millisecond},
			{id: 2, url: "https://example.com/detail/2", duration: 12 * time.Millisecond},
			{id: 3, url: "https://example.com/detail/3", duration: 25 * time.Millisecond},
			{id: 4, url: "https://example.com/detail/4", duration: 40 * time.Millisecond},
			{id: 5, url: "https://example.com/detail/5", duration: 7 * time.Millisecond},
			{id: 6, url: "https://example.com/detail/6", duration: 33 * time.Millisecond},
		}),
		"video": newFakeStage(t, "video", 4, []fakeTask{
			{id: 1, url: "https://example.com/video/1", duration: 50 * time.Millisecond},
			{id: 2, url: "https://example.com/video/2", duration: 20 * time.Millisecond, fail: true},
			{id: 3, url: "https://example.com/video/3", duration: 60 * time.Millisecond},
			{id: 4, url: "https://example.com/video/4", duration: 45 * time.Millisecond},
			{id: 5, url: "https://example.com/video/5", duration: 15 * time.Millisecond},
			{id: 6, url: "https://example.com/video/6", duration: 35 * time.Millisecond},
			{id: 7, url: "https://example.com/video/7", duration: 28 * time.Millisecond},
			{id: 8, url: "https://example.com/video/8", duration: 55 * time.Millisecond, fail: true},
			{id: 9, url: "https://example.com/video/9", duration: 10 * time.Millisecond},
			{id: 10, url: "https://example.com/video/10", duration: 42 * time.Millisecond},
		}),
	}

	reg := dataadmin.New(nil)
	_ = reg.Register("users", "用户", &fakeUser{})
	_ = reg.Register("orders", "订单", &fakeOrder{})

	return NewMonitor(
		func() map[string]*track.StatsQueue[*crawler.Task] { return stages },
		testLogger{t: t},
		MonitorConfig{
			Metrics:   fakeMetrics(),
			DataAdmin: reg,
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

func emptyGetter() map[string]*track.StatsQueue[*crawler.Task] {
	return map[string]*track.StatsQueue[*crawler.Task]{}
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
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{AuthKey: "s3cret"})

	cases := []struct {
		name   string
		target string
		header map[string]string
		want   int
	}{
		{"no key", "/api/monitor", nil, http.StatusUnauthorized},
		{"wrong key", "/api/monitor", map[string]string{"X-Auth-Key": "nope"}, http.StatusUnauthorized},
		{"bearer", "/api/monitor", map[string]string{"Authorization": "Bearer s3cret"}, http.StatusOK},
		{"x-auth-key", "/api/monitor", map[string]string{"X-Auth-Key": "s3cret"}, http.StatusOK},
		{"query key", "/api/monitor?key=s3cret", nil, http.StatusOK},
		{"html no auth", "/monitor", nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			m.Register(mux)
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

func TestSecretHandler(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "secret.txt")

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{AuthKeyFile: kf})
	mux := http.NewServeMux()
	m.Register(mux)

	// 生成前无密钥，接口开放
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/monitor", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("before secret status = %d, want 200", rr.Code)
	}

	// 重新生成密钥
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/api/settings/secret", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("POST secret status = %d, body=%s", rr2.Code, rr2.Body.String())
	}
	key := decodeJSON(t, rr2)["key"].(string)
	if len(key) != 64 {
		t.Errorf("secret length = %d, want 64", len(key))
	}
	if b, err := os.ReadFile(kf); err != nil {
		t.Fatalf("read secret file: %v", err)
	} else if !strings.Contains(string(b), key) {
		t.Errorf("secret file content = %q, want contains %q", string(b), key)
	}

	// 生成后需新密钥访问
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, httptest.NewRequest(http.MethodGet, "/api/monitor", nil))
	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("after secret no key status = %d, want 401", rr3.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	req.Header.Set("X-Auth-Key", key)
	rr4 := httptest.NewRecorder()
	mux.ServeHTTP(rr4, req)
	if rr4.Code != http.StatusOK {
		t.Errorf("after secret with key status = %d, want 200", rr4.Code)
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

func TestDataModels(t *testing.T) {
	// 未启用 DataAdmin
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	if rr := serve(m, http.MethodGet, "/api/data/models", nil); rr.Code != http.StatusNotFound {
		t.Errorf("nil dataadmin status = %d, want 404", rr.Code)
	}

	// 启用后列出模型
	m2 := newFakeMonitor(t)
	rr := serve(m2, http.MethodGet, "/api/data/models", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	models := decodeJSON(t, rr)["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models count = %d, want 2", len(models))
	}
}

func TestDataList(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	if rr := serve(m, http.MethodGet, "/api/data/users", nil); rr.Code != http.StatusNotFound {
		t.Errorf("nil dataadmin list status = %d, want 404", rr.Code)
	}

	m2 := newFakeMonitor(t)
	if rr := serve(m2, http.MethodGet, "/api/data/nope", nil); rr.Code != http.StatusNotFound {
		t.Errorf("unknown model status = %d, want 404", rr.Code)
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

func TestExtractKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/monitor", nil)
	if extractKey(req) != "" {
		t.Error("empty key should be empty")
	}
	req.Header.Set("Authorization", "Bearer tok")
	if extractKey(req) != "tok" {
		t.Error("bearer key not extracted")
	}
	req.Header.Del("Authorization")
	req.Header.Set("X-Auth-Key", "xk")
	if extractKey(req) != "xk" {
		t.Error("x-auth-key not extracted")
	}
	req.Header.Del("X-Auth-Key")
	req = httptest.NewRequest(http.MethodGet, "/api/monitor?key=qk", nil)
	if extractKey(req) != "qk" {
		t.Error("query key not extracted")
	}
}

func TestParseFilter(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/data/users?filter[age]=30&filter[name]=bob&page=1", nil)
	f := parseFilter(req)
	if f["age"] != "30" || f["name"] != "bob" {
		t.Errorf("filter = %v", f)
	}
	if _, ok := f["page"]; ok {
		t.Error("non-filter query should be ignored")
	}
}

func TestAtoiDefault(t *testing.T) {
	if atoiDefault("", 5) != 5 || atoiDefault("x", 5) != 5 || atoiDefault("42", 5) != 42 {
		t.Error("atoiDefault behavior wrong")
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

func TestGenerateSecret(t *testing.T) {
	s, err := generateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 64 {
		t.Errorf("secret length = %d, want 64", len(s))
	}
}

// TestMonitorDemoJSON 把完整假数据接口输出落盘并打印，方便直观查看效果。
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
