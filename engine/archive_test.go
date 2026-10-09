package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

/* ---------- 辅助 ---------- */

// archiveEngine 造一个开了归档的引擎：stub 库（trace 走它能读回 SQL）+ HTML 客户端 + 归档目录。
func archiveEngine(t *testing.T, mode string) (*Engine, *stubConnPool, string) {
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
	e := &Engine{db: db, loggerSet: testLoggerSet(), cfg: &config.Config{}}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	e.runtime.Store(&config.RuntimeConfig{})

	e.cfg.Crawler.Trace.Enabled = true
	e.cfg.HTML = config.HTMLConfig{Enable: true, Timeout: 3 * time.Second, MaxBodySize: 4 << 20}
	e.cfg.Crawler.Archive = config.ArchiveConfig{Enabled: true, Dir: dir, Mode: mode}
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

// listArchive 列出归档目录下的所有文件（相对路径，斜杠统一成 /）。
func listArchive(t *testing.T, dir string) []string {
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
		t.Fatalf("列归档目录：%v", err)
	}
	return out
}

/* ---------- 落盘时机与内容 ---------- */

// 失败的尝试要留下**原样字节**：不做任何再序列化（goquery 走一遍会丢 noscript 里的回退标签），
// 文件名带上 try（= task.Retry），trace 里留一条指向文件的步骤。
func TestArchiveWritesRawBytesOnFailedAttempt(t *testing.T) {
	// 这段 HTML 刻意包含：注释、noscript 回退、连续空格 —— 任何"重新序列化"都会改动它们
	const body = `<html><head><title>t</title></head><body><!-- keep -->` +
		`<noscript><img src="/lazy-cover.webp"></noscript>  多余的空格  </body></html>`
	srv := serveBody(t, body)

	e, pool, dir := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 7, Stage: "stub", URL: srv.URL}
	f := &pageFetcher{urls: []string{srv.URL}, err: errors.New("解析失败：选择器没匹配到")}

	if err := e.runAttempt(context.Background(), f, task, 0); err == nil {
		t.Fatal("handler 的错误应原样返回")
	}

	files := listArchive(t, dir)
	var htmlRel string
	for _, rel := range files {
		if strings.HasSuffix(rel, ".html") {
			htmlRel = rel
		}
	}
	if htmlRel == "" {
		t.Fatalf("失败的尝试应当留下归档，实得 %v", files)
	}
	// 路径形状：{stage}/task-{id}-try-{retry}-{urlhash8}.html
	if !regexp.MustCompile(`^stub/task-7-try-0-[0-9a-f]{8}\.html$`).MatchString(htmlRel) {
		t.Fatalf("归档路径形状不对：%q", htmlRel)
	}
	// 旁边的元信息
	metaRel := strings.TrimSuffix(htmlRel, ".html") + ".json"
	if !slices.Contains(files, metaRel) {
		t.Fatalf("应当同时留下元信息 %q，实得 %v", metaRel, files)
	}

	got, err := os.ReadFile(filepath.Join(dir, htmlRel))
	if err != nil {
		t.Fatalf("读归档：%v", err)
	}
	if string(got) != body {
		t.Fatalf("归档的必须是原样字节（不是再序列化的结果）：\n实得 %q\n期望 %q", got, body)
	}

	meta, err := os.ReadFile(filepath.Join(dir, metaRel))
	if err != nil {
		t.Fatalf("读元信息：%v", err)
	}
	for _, want := range []string{`"task_id":7`, `"stage":"stub"`, `"status":200`, `"final_url":"` + srv.URL} {
		if !strings.Contains(string(meta), want) {
			t.Fatalf("元信息里缺少 %s：%s", want, meta)
		}
	}

	// trace 里要有指向文件的那一步 —— 否则"失败了"和"那一页"连不起来
	trace := pool.all()
	if !strings.Contains(trace, archiveTraceStep) {
		t.Fatalf("trace 里应有一条 %q 步骤：\n%s", archiveTraceStep, trace)
	}
	if !strings.Contains(trace, filepath.Base(htmlRel)) {
		t.Fatalf("这条步骤里应带上归档文件名：\n%s", trace)
	}
}

// 默认模式（failure）下成功的尝试不落盘：写入量按失败率走，与 trace「只在失败尝试写 data」同构。
func TestArchiveSkipsSuccessfulAttemptByDefault(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, pool, dir := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 7, Stage: "stub", URL: srv.URL}

	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, task, 0); err != nil {
		t.Fatalf("err = %v", err)
	}
	if files := listArchive(t, dir); len(files) != 0 {
		t.Fatalf("成功的尝试不该落盘，实得 %v", files)
	}
	if strings.Contains(pool.all(), archiveTraceStep) {
		t.Fatalf("没归档就不该写归档步骤：\n%s", pool.all())
	}
}

// always 模式：每次尝试都落（排查期开）。
func TestArchiveAlwaysModeWritesOnSuccess(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways)
	task := &Task{ID: 7, Stage: "stub", URL: srv.URL}

	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, task, 0); err != nil {
		t.Fatalf("err = %v", err)
	}
	if files := listArchive(t, dir); len(files) != 2 { // .html + .json
		t.Fatalf("always 模式成功的尝试也要落盘，实得 %v", files)
	}
}

// handler panic 时也要落盘（那时命名返回值 err 还是 nil，得靠 recover 拿到"出事了"这个信号），
// 且 panic 必须原样抛给 workerpool。
func TestArchiveOnPanic(t *testing.T) {
	srv := serveBody(t, "<html>boom</html>")
	e, pool, dir := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 8, Stage: "stub", URL: srv.URL}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("panic 必须照旧抛出")
		}
		files := listArchive(t, dir)
		if len(files) != 2 {
			t.Fatalf("panic 的尝试应当落盘，实得 %v", files)
		}
		if !strings.Contains(pool.all(), archiveTraceStep) {
			t.Fatalf("panic 路径也要把归档写进 trace：\n%s", pool.all())
		}
	}()

	_ = e.runAttempt(context.Background(),
		&pageFetcher{urls: []string{srv.URL}, panicWith: "handler 炸了"}, task, 0)
}

// 重试不能覆盖上一次的现场：文件名里的 try 取 task.Retry，
// 而"第一次是登录墙、第三次正常"恰好是排查的关键信息。
func TestArchiveKeepsEveryAttemptSeparate(t *testing.T) {
	srv := serveBody(t, "<html>wall</html>")
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 9, Stage: "stub", URL: srv.URL}

	f := func() *pageFetcher { return &pageFetcher{urls: []string{srv.URL}, err: errors.New("boom")} }
	_ = e.runAttempt(context.Background(), f(), task, 0)
	task.Retry = 1
	_ = e.runAttempt(context.Background(), f(), task, 1)

	var htmls []string
	for _, rel := range listArchive(t, dir) {
		if strings.HasSuffix(rel, ".html") {
			htmls = append(htmls, rel)
		}
	}
	if len(htmls) != 2 {
		t.Fatalf("两次尝试应各留一份，实得 %v", htmls)
	}
	if !strings.Contains(strings.Join(htmls, " "), "try-0-") || !strings.Contains(strings.Join(htmls, " "), "try-1-") {
		t.Fatalf("文件名应带上 try：%v", htmls)
	}
}

// 同一个 URL 在一次尝试里抓了两次：留最后那一次（handler 重抓通常是因为前一次不可用），
// 不留下两个同名文件互相覆盖。
func TestArchiveSameURLTwiceKeepsOneFile(t *testing.T) {
	srv := serveBody(t, "<html>twice</html>")
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways)
	task := &Task{ID: 10, Stage: "stub", URL: srv.URL}

	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL, srv.URL}}, task, 0); err != nil {
		t.Fatalf("err = %v", err)
	}
	if files := listArchive(t, dir); len(files) != 2 { // 一份 .html + 一份 .json
		t.Fatalf("同一个 URL 只该留一份，实得 %v", files)
	}
}

// 超过单页上限的页面不归档（判据在**登记**时就生效，顺带把内存兜住）。
func TestArchiveSkipsPageOverLimit(t *testing.T) {
	srv := serveBody(t, strings.Repeat("x", (1<<20)+1)) // 1MB + 1 字节
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways)
	e.cfg.HTML.MaxBodySize = 4 << 20 // 抓得下来，但归档上限是 1MB
	e.cfg.Crawler.Archive.MaxFileMB = 1
	e.SetHTMLClient()

	task := &Task{ID: 11, Stage: "stub", URL: srv.URL}
	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, task, 0); err != nil {
		t.Fatalf("err = %v", err)
	}
	if files := listArchive(t, dir); len(files) != 0 {
		t.Fatalf("超过上限的页面不该归档，实得 %v", files)
	}
}

// 没开归档时：FetchHTML 照常工作，一个文件都不写（所有归档方法对 nil 安全）。
func TestArchiveDisabledIsNoop(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, pool, dir := archiveEngine(t, config.ArchiveModeAlways)
	e.cfg.Crawler.Archive.Enabled = false
	e.cfg.Crawler.Archive.Dir = ""

	task := &Task{ID: 12, Stage: "stub", URL: srv.URL}
	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}, err: errors.New("boom")}, task, 0); err == nil {
		t.Fatal("handler 的错误应原样返回")
	}
	if files := listArchive(t, dir); len(files) != 0 {
		t.Fatalf("归档关着就不该写文件，实得 %v", files)
	}
	if strings.Contains(pool.all(), archiveTraceStep) {
		t.Fatalf("归档关着就不该写归档步骤：\n%s", pool.all())
	}
}

/* ---------- 保留期清理 ---------- */

// 清理按文件的修改时间判：过期的删掉、没过期的留着。
func TestCleanupArchiveDeletesExpired(t *testing.T) {
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	oldFile := filepath.Join(dir, "stub", "task-1-try-0-deadbeef.html")
	freshFile := filepath.Join(dir, "stub", "task-2-try-0-feedface.html")
	if err := os.MkdirAll(filepath.Dir(oldFile), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{oldFile, freshFile} {
		if err := os.WriteFile(p, []byte("<html></html>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}

	if n := e.cleanupArchive(24 * time.Hour); n != 1 {
		t.Fatalf("应当只删过期的那一个，实得 %d", n)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("过期的归档没被删掉")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatalf("没过期的归档不该被删：%v", err)
	}
}

// 渲染路径（FetchRendered）同样要归档：那份 HTML 本来就在手上，登记不额外花 CDP 调用。
// 这里直接测缓冲这一层 —— 真正的 FetchRendered 要 Chrome/CDP，离线跑不了（与仓库里既有的
// "pkg/browser 相关路径需要 CDP" 那几个洞一致）。
func TestArchiveHoldsRenderedPage(t *testing.T) {
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	ar := e.newArchiveBuffer(&Task{ID: 20, Stage: "stub", URL: "https://example.com"}, 0)
	if ar == nil {
		t.Fatal("开了归档就该有缓冲")
	}
	ar.holdRendered(`<html><body>rendered</body></html>`, "https://example.com/final")
	if files := ar.finish(true); len(files) != 1 {
		t.Fatalf("渲染页应当被归档，实得 %v", files)
	}

	var htmlRel string
	for _, rel := range listArchive(t, dir) {
		if strings.HasSuffix(rel, ".html") {
			htmlRel = rel
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, htmlRel))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `<html><body>rendered</body></html>` {
		t.Fatalf("渲染页也要原样落盘，实得 %q", b)
	}
	// 渲染路径没有状态码/Content-Type，元信息里留零值而不是假装有
	meta, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(htmlRel, ".html")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"final_url":"https://example.com/final"`) {
		t.Fatalf("元信息应记最终 URL：%s", meta)
	}
	if !strings.Contains(string(meta), `"status":0`) {
		t.Fatalf("渲染路径的状态码应为 0：%s", meta)
	}
}

/* ---------- 请求头：站点级 + 逐请求 ---------- */

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

	e, _, _ := archiveEngine(t, config.ArchiveModeFailure) // 复用：配好 HTML 客户端 + stub 库
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

	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
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

/* ---------- 代理：随用随取 ---------- */

// 出口由 fetcher 决定：`engine.NextProxy()` 取一个、`papa.WithProxyURL` 传进去。
// 没配代理管理器时取到空串（"没代理"与"池子暂时为空"对调用方是同一种处理：直连）。
func TestNextProxyWithoutManager(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	if got := e.NextProxy(); got != "" {
		t.Fatalf("没配代理管理器时应返回空串，实得 %q", got)
	}
	e.SetProxy(proxy.NewManager("", time.Hour))
	if got := e.NextProxy(); got != "" {
		t.Fatalf("管理器没有可用代理时应返回空串，实得 %q", got)
	}
}

// 浏览器路径**做不到**逐请求代理（代理是浏览器实例级设置），传了要**明确报错**而不是静默直连 ——
// 静默的话请求会从别的出口出去，而调用方以为走了代理。
func TestFetchRenderedRejectsProxyURL(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	ctx := core.WithProxyURL(context.Background(), "http://1.2.3.4:8080")

	_, _, err := e.FetchRendered(ctx, "https://example.com", "")
	if err == nil {
		t.Fatal("传了代理地址应当报错")
	}
	if !strings.Contains(err.Error(), "不支持逐请求代理地址") {
		t.Fatalf("报错要说清原因与替代路，实得：%v", err)
	}
}
