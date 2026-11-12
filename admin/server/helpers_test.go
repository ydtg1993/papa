package server

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/core"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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

// Explicit authorization fixture for handler tests; production has no anonymous fallback.
func allowUnrestrictedTestRequests(*http.Request) (string, bool) { return "", true }

// newFakeMonitor 组装一个带全套假数据的监控路由。
func newFakeMonitor(t *testing.T) *Monitor {
	t.Helper()

	return NewMonitor(
		fakeStageStats,
		testLogger{t: t},
		MonitorConfig{VerifyToken: allowUnrestrictedTestRequests},
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
