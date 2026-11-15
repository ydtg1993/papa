package engine

import (
	"context"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

/* ---------- 辅助 ---------- */

// snapshotEngine 造一个开了页面快照的引擎：stub 库（任务计时走假库，trace 写文件）+ HTML 客户端 + 页面快照目录。
func snapshotEngine(t *testing.T, mode config.TraceMode, origins ...string) (*Engine, *stubConnPool, string) {
	t.Helper()
	pool := &stubConnPool{}
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open stub db: %v", err)
	}

	dir := t.TempDir()
	e := &Engine{db: db, loggerSet: testLoggerSet(), cfg: &config.Config{Network: egress.Config{PrivateOrigins: origins}}}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)

	e.cfg.Crawler.Trace.Mode = mode
	e.cfg.Crawler.TraceFiles.Dir = dir
	e.cfg.HTML = config.HTMLConfig{Enable: true, Timeout: 3 * time.Second, MaxBodySize: 4 << 20}
	e.SetHTMLClient()
	return e, pool, dir
}

// pageFetcher 走 engine.FetchHTML 抓给定 URL（真实 handler 的写法），再按设定收场。
type pageFetcher struct {
	urls      []string
	err       error
	panicWith any
	fetchErr  error
	// wrap 可选：包一层 ctx（用来模拟 handler 里 `engine.FetchHTML(papa.WithHeaders(ctx, …), url)`）
	wrap func(context.Context) context.Context
}

func (f *pageFetcher) GetStage() string { return "stub" }

func (f *pageFetcher) FetchHandler(ctx context.Context, _ *Task, e *Engine) error {
	if f.wrap != nil {
		ctx = f.wrap(ctx)
	}
	for _, u := range f.urls {
		if _, err := e.FetchHTML(ctx, u); err != nil {
			f.fetchErr = err
		}
	}
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	return f.err
}

// serveBody 起一个只会返回固定内容的服务器。
func serveBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// listEvidenceFiles 列出页面快照目录下的所有文件（相对路径，斜杠统一成 /）。
func listEvidenceFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("列页面快照目录：%v", err)
	}
	return out
}

// headerRecorder 起一个把收到的请求头记下来的服务器。
func headerRecorder(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var last http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		if last == nil {
			return http.Header{}
		}
		return last.Clone()
	}
}

// 站点级的请求头**自动**跟着任务走：同一个引擎里两个站各带各的 UA/Cookie，业务不用写代码
// （多站同进程要"各站一套请求头"就靠这条）。逐请求的 `papa.WithHeaders` 在它之上再覆盖。
func TestSiteHeadersFollowTheTask(t *testing.T) {
	srv, seen := headerRecorder(t)

	e, _, _ := snapshotEngine(t, config.TraceErrors, srv.URL) // 复用：配好 HTML 客户端 + stub 库
	e.cfg.HTML.Headers = map[string]string{"X-From-Config": "cfg", "X-Keep": "keep"}
	e.SetHTMLClient()
	e.SetSite(core.Site{Key: "a", Headers: map[string]string{"User-Agent": "ua-a", "Cookie": "sid=a"}})
	e.SetSite(core.Site{Key: "b", Headers: map[string]string{"User-Agent": "ua-b"}})

	// 站点 a 的任务
	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, &Task{ID: 1, Stage: "stub", URL: srv.URL, Site: "a"}, 0); err != nil {
		t.Fatalf("runAttempt = %v", err)
	}
	got := seen()
	if got.Get("User-Agent") != "ua-a" || got.Get("Cookie") != "sid=a" {
		t.Fatalf("站点 a 的请求头没带上：%v", got)
	}
	if got.Get("X-From-Config") != "cfg" || got.Get("X-Keep") != "keep" {
		t.Fatalf("全局配置头不该被站点级丢掉：%v", got)
	}

	// 站点 b 的任务：同一进程、同一客户端，换成 b 那套
	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, &Task{ID: 2, Stage: "stub", URL: srv.URL, Site: "b"}, 0); err != nil {
		t.Fatalf("runAttempt = %v", err)
	}
	got = seen()
	if got.Get("User-Agent") != "ua-b" {
		t.Fatalf("站点 b 应当用自己的 UA，实得 %q", got.Get("User-Agent"))
	}
	if _, ok := got["Cookie"]; ok {
		t.Fatalf("站点 b 不该带上站点 a 的 Cookie：%v", got)
	}
}

// 逐请求头在站点级之上叠加：只覆盖给到的键，其余（含站点级、全局配置）留着。
func TestPerRequestHeadersOverrideSiteHeaders(t *testing.T) {
	srv, seen := headerRecorder(t)

	e, _, _ := snapshotEngine(t, config.TraceErrors, srv.URL)
	e.SetHTMLClient()
	e.SetSite(core.Site{Key: "a", Headers: map[string]string{"User-Agent": "ua-a", "Referer": "https://site-a/"}})

	// handler 里就这么写：`engine.FetchHTML(papa.WithHeaders(ctx, …), url)`
	f := &pageFetcher{urls: []string{srv.URL}}
	f.wrap = func(ctx context.Context) context.Context {
		return core.WithHeaders(ctx, map[string]string{"Referer": "https://one-off/"})
	}
	if err := e.runAttempt(context.Background(), f, &Task{ID: 3, Stage: "stub", URL: srv.URL, Site: "a"}, 0); err != nil {
		t.Fatalf("runAttempt = %v", err)
	}
	got := seen()
	if got.Get("Referer") != "https://one-off/" {
		t.Fatalf("逐请求应当覆盖站点级：%q", got.Get("Referer"))
	}
	if got.Get("User-Agent") != "ua-a" {
		t.Fatalf("没提到的站点级头要留着：%q", got.Get("User-Agent"))
	}
}

// 浏览器路径**做不到**逐请求代理（代理是浏览器实例级设置），传了要**明确报错**而不是静默直连 ——
// 静默的话请求会从别的出口出去，而调用方以为走了代理。
func TestFetchRenderedRejectsProxyURL(t *testing.T) {
	e, _, _ := snapshotEngine(t, config.TraceErrors)
	ctx := core.WithProxyURL(context.Background(), "http://1.2.3.4:8080")

	_, _, err := e.FetchRendered(ctx, "https://example.com", "")
	if err == nil {
		t.Fatal("传了代理地址应当报错")
	}
	if !strings.Contains(err.Error(), "不支持逐请求代理地址") {
		t.Fatalf("报错要说清原因与替代路，实得：%v", err)
	}
}
