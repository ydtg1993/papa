package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
)

/* ---------- 门面 getter / setter ---------- */

// 引擎是业务的唯一入口：这些 setter 必须在 ApplyRegisterStage 之前可用，
// 且 getter 返回的就是同一个对象（业务会拿回去自己用）。
func TestEngineAccessorsRoundTrip(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	if e.GetDB() != e.db {
		t.Fatal("GetDB 应返回同一个 *gorm.DB")
	}
	if e.GetLoggerSet() != e.loggerSet {
		t.Fatal("GetLoggerSet 应返回同一个 LoggerSet")
	}
	if e.GetConfig() != e.cfg {
		t.Fatal("GetConfig 应返回同一份配置")
	}

	if e.GetProxy() != nil || e.GetM3U8() != nil || e.GetFiledown() != nil {
		t.Fatal("没设置过的中间件应当是 nil")
	}

	pm := proxy.NewManager("", time.Hour)
	e.SetProxy(pm)
	if e.GetProxy() != pm {
		t.Fatal("SetProxy/GetProxy 不是同一个对象")
	}
	// 下载器没有全局 setter（配置只写站点/阶段两层）：框架用 InitDownloaders 备好基座，
	// GetM3U8/GetFiledown 返回的就是那一份。
	e.InitDownloaders()
	if e.GetM3U8() == nil || e.GetFiledown() == nil {
		t.Fatal("InitDownloaders 之后两个下载器都该就绪")
	}
}

// Errors 返回引擎的常驻错误通道（单条）：所有池（含懒建的）都汇到这一条，不再是启动时逐个池快照。
func TestErrorsReturnsPersistentChannel(t *testing.T) {
	e := &Engine{errorCh: make(chan error, 100)}
	chans := e.Errors()
	if len(chans) != 1 || chans[0] == nil {
		t.Fatalf("应返回单条非 nil 错误通道，实得 %v", chans)
	}
}

// 池的错误要能汇进引擎常驻通道：转发协程把 pool.Errors() 里的错误搬进 errorCh。
func TestPoolErrorForwarderForwards(t *testing.T) {
	e := &Engine{
		ctx:                context.Background(),
		loggerSet:          testLoggerSet(),
		errorCh:            make(chan error, 100),
		errorForwardCancel: make(map[string]*poolForwarder),
	}

	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e.startPoolErrorForwarder("stub", pool)

	// 停掉的池上 Submit 会 SendError("worker pool already stopped")
	pool.Stop(0)
	_ = pool.Submit(&Task{Stage: "stub", URL: "https://example.com"})

	select {
	case err := <-e.errorCh:
		if !strings.Contains(err.Error(), "already stopped") {
			t.Fatalf("应转发池错误，实得：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("1s 内没收到转发错误")
	}
}

/* ---------- Upsert ---------- */

// Upsert 落库后主键已被回填（插入路径），所以不需要再回查。
func TestUpsertInsertsAndBackfills(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	rec := &crawlerTaskStub{URL: "https://example.com/upsert", Stage: "stub"}
	err := e.Upsert(rec, []string{"url", "stage"}, []string{"title"})
	if err != nil {
		t.Fatalf("Upsert = %v", err)
	}
	if !containsAll(f.written(), "INSERT") {
		t.Fatalf("应执行 INSERT ... ON DUPLICATE KEY UPDATE：\n%s", f.written())
	}
	if !containsAll(f.written(), "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("应带上冲突更新子句：\n%s", f.written())
	}
	if rec.ID == 0 {
		t.Fatal("主键应被回填")
	}
	// 插入路径主键已回填，不该再回查
	if f.readSQL() != "" {
		t.Fatalf("插入路径不该回查：\n%s", f.readSQL())
	}
}

/* ---------- 浏览器 / HTML 客户端 ---------- */

// 关掉开关时不建任何东西：GetBrowserPool / GetHTMLClient 返回 nil，
// 业务据此走"没配浏览器"的分支（FetchRendered 会给出明确错误而不是崩）。
func TestSetPoolsSkippedWhenDisabled(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}

	e.SetBrowserPool()
	e.SetHTMLClient()

	if e.GetBrowserPool() != nil {
		t.Fatal("browser.enable=false 时不该建池子")
	}
	if e.GetHTMLClient() != nil {
		t.Fatal("html.enable=false 时不该建客户端")
	}
}

func TestSetHTMLClientWhenEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.HTML.Enable = true
	cfg.HTML.Timeout = 5 * time.Second
	cfg.HTML.MaxBodySize = 2048
	e := &Engine{cfg: cfg}

	e.SetHTMLClient()
	if e.GetHTMLClient() == nil {
		t.Fatal("html.enable=true 时应建客户端")
	}
}

// 池子是按需建实例的，这里不配 max_idle_time（也就不起回收协程），
// 所以建池这一步完全离线、不会去拉 Chrome。
func TestSetBrowserPoolWhenEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Browser.Enable = true
	cfg.Browser.PoolSize = 1
	cfg.Browser.MaxIdleTime = 0
	e := &Engine{cfg: cfg}

	e.SetBrowserPool()
	pool := e.GetBrowserPool()
	if pool == nil {
		t.Fatal("browser.enable=true 时应建池子")
	}
	defer pool.Close()

	// 同一份配置再建一次也不该炸（业务可能在测试里反复调）
	e.SetBrowserPool()
	if e.GetBrowserPool() == pool {
		t.Fatal("应替换成新建的池子")
	}
	e.GetBrowserPool().Close()
}

// 没配浏览器池时 FetchRendered 给一条能看懂的错误，而不是 nil 解引用。
func TestFetchRenderedWithoutPool(t *testing.T) {
	e := &Engine{}
	if _, _, err := e.FetchRendered(t.Context(), "https://example.com", ""); err == nil {
		t.Fatal("没配浏览器池应当报错")
	} else if !containsAll(err.Error(), "browser") {
		t.Fatalf("错误信息应指向浏览器未启用：%v", err)
	}
}

// 渲染路径也要能拿到 *htmlfetch.Page：没配池子时给出的错误与 FetchRendered 同一条。
func TestFetchRenderedPageWithoutPool(t *testing.T) {
	e := &Engine{}
	if _, err := e.FetchRenderedPage(t.Context(), "https://example.com", ""); err == nil {
		t.Fatal("没配浏览器池应当报错")
	} else if !containsAll(err.Error(), "browser") {
		t.Fatalf("错误信息应指向浏览器未启用：%v", err)
	}
}

// 渲染出来的 Page 必须能**直接**喂进 engine.RestrictedError —— 这正是这条 API 存在的理由：
// FetchRendered 给的是 *goquery.Document，而受限页判据（默认词表 + 站点词表）收的是
// *htmlfetch.Page，于是走浏览器的阶段被反爬拦下时只表现为"解析不出东西"。
//
// 不借真 Chrome：判据看的是 Page.Document，htmlfetch.NewRenderedPage 造的页与引擎渲染出来的页
// 在这一点上完全同类（真 Chrome 那条端到端见 TestFetchRenderedPageEndToEnd）。
func TestRenderedPageFeedsRestrictedError(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	e.SetSite(core.Site{Key: "s", RestrictedKeywords: []string{"本站专属拦截话术"}})

	interstitial, err := htmlfetch.NewRenderedPage("https://example.com/drama/1/",
		`<html><head><title>Just a moment...</title></head><body><p>Checking your browser before accessing</p></body></html>`)
	if err != nil {
		t.Fatalf("NewRenderedPage = %v", err)
	}
	err = e.RestrictedError(&Task{ID: 1, Stage: "catalog", Site: "s"}, interstitial, "")
	if err == nil {
		t.Fatal("命中默认词表的渲染页应当被判成受限页")
	}
	if core.Retryable(err) {
		t.Fatal("受限页必须不可重试")
	}
	if kind := core.ErrorKind(err); kind != "access_restricted" {
		t.Fatalf("分类 = %q, want access_restricted", kind)
	}
	if !strings.Contains(err.Error(), "https://example.com/drama/1/") {
		t.Fatalf("消息里应当带上页面地址：%v", err)
	}

	// 站点词表在渲染路径上同样生效（两路用的是同一套判据），干净页返回 nil
	siteHit, err := htmlfetch.NewRenderedPage("https://example.com/drama/2/",
		`<html><body><h1>本站专属拦截话术</h1></body></html>`)
	if err != nil {
		t.Fatalf("NewRenderedPage = %v", err)
	}
	if err := e.RestrictedError(&Task{ID: 2, Stage: "catalog", Site: "s"}, siteHit, ""); err == nil {
		t.Fatal("站点词表命中了却没报错")
	}
	clean, err := htmlfetch.NewRenderedPage("https://example.com/drama/3/", `<html><body><h1>正常页面</h1></body></html>`)
	if err != nil {
		t.Fatalf("NewRenderedPage = %v", err)
	}
	if err := e.RestrictedError(&Task{ID: 3, Stage: "catalog", Site: "s"}, clean, ""); err != nil {
		t.Fatalf("干净页不该判受限：%v", err)
	}
}

// 真 Chrome 端到端：FetchRenderedPage 交出可用的 Page（能查询、URL 是导航后的地址），
// 顺带确认抽出 render() 之后 FetchRendered 这条路没变。没有浏览器就跳过（与 pkg/browser 同取向）。
func TestFetchRenderedPageEndToEnd(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有可用浏览器，跳过（这条要真 Chrome）")
	}
	srv := serveBody(t, `<html><head><title>渲染页</title></head><body><h1 class="hit">等 JS 才有的内容</h1></body></html>`)

	cfg := &config.Config{}
	cfg.Browser.Enable = true
	cfg.Browser.PoolSize = 1
	cfg.Browser.Headless = true
	cfg.Browser.MaxIdleTime = 0
	e := &Engine{cfg: cfg}
	e.SetBrowserPool()
	defer e.GetBrowserPool().Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	page, err := e.FetchRenderedPage(ctx, srv.URL, "h1.hit")
	if err != nil {
		if strings.Contains(err.Error(), "acquire browser") {
			t.Skipf("拿不到浏览器实例（本机浏览器起不来），跳过：%v", err)
		}
		t.Fatalf("FetchRenderedPage = %v", err)
	}
	if page == nil || page.HTML == "" || page.Document == nil {
		t.Fatalf("渲染页应当是解析好的 Page，实得 %+v", page)
	}
	if got, ok := page.Document.Text("h1.hit"); !ok || !strings.Contains(got, "等 JS 才有的内容") {
		t.Fatalf("Page 应当能直接查询：%q %v", got, ok)
	}
	// 比的是"最终地址"不是"发出的地址"：Chrome 会把裸 origin 规范成带尾斜杠的形态
	// （请求 http://host:port → 最终 URL http://host:port/），所以两边都去掉尾斜杠再比。
	if page.URL == nil || strings.TrimSuffix(page.URL.String(), "/") != strings.TrimSuffix(srv.URL, "/") {
		t.Fatalf("URL = %v, want %s（导航后的最终地址）", page.URL, srv.URL)
	}
	if page.StatusCode != 0 || page.ContentType != "" {
		t.Fatalf("渲染路径拿不到状态码/Content-Type，应当是零值：%d/%q", page.StatusCode, page.ContentType)
	}
	if err := e.RestrictedError(&Task{ID: 1, Stage: "catalog"}, page, ""); err != nil {
		t.Fatalf("干净页不该判受限：%v", err)
	}

	// 同一个 render() 的另一条出口：拆完之后 FetchRendered 仍然照旧
	doc, finalURL, err := e.FetchRendered(ctx, srv.URL, "h1.hit")
	if err != nil {
		t.Fatalf("FetchRendered = %v", err)
	}
	if strings.TrimSuffix(finalURL, "/") != strings.TrimSuffix(srv.URL, "/") || doc == nil ||
		!strings.Contains(doc.Text(), "等 JS 才有的内容") {
		t.Fatalf("FetchRendered 的返回值变了：finalURL=%q doc=%v", finalURL, doc)
	}
}

/* ---------- 去重表装载 ---------- */

// 启动时把「未到终态」与全部轮询任务装进内存去重表；
// 已完成的历史任务不装（否则内存随历史任务无限长），由 DB 唯一索引兜底。
func TestLoadActiveTasksFillsDedupCache(t *testing.T) {
	f := newFakeTaskDB()
	f.row["url"] = "https://example.com/active"
	f.row["stage"] = "stub"
	e := &Engine{
		db:         openFakeTaskDB(t, f),
		loggerSet:  &loggers.LoggerSet{DB: quietLogger()},
		dedupCache: newDedupCache(0),
	}

	e.loadActiveTasks()

	// 键是 `site:stage:url`，含站点 —— 装载时照抄旧格式（或漏了 Site）就会装进一个永远命不中的键，
	// 静默退化成"全靠查库"。假库那行的 site 是 stub，所以键是 stub:stub:...
	if !e.dedupCache.Get("stub:stub:https://example.com/active") {
		t.Fatal("进行中的任务应被装进去重表（键含站点）")
	}
	// 装的是 pending/processing + repeatable，条件必须写进查询
	read := f.readSQL()
	if !containsAll(read, "status IN") || !containsAll(read, "repeatable") {
		t.Fatalf("装载条件不完整：\n%s", read)
	}
	// 只投影去重键需要的列：历史行宽了以后 SELECT * 会把用不上的列也读出来
	if strings.Contains(read, "SELECT *") {
		t.Fatalf("预热不该 SELECT *：\n%s", read)
	}
	for _, col := range []string{"`url`", "`stage`", "`site`", "`idempotency_key`"} {
		if !strings.Contains(read, col) {
			t.Fatalf("预热查询缺列 %s：\n%s", col, read)
		}
	}
}

// 库里一条进行中的任务都没有时：缓存保持空，不 panic。
func TestLoadActiveTasksWithNoActiveRows(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := &Engine{
		db:         openFakeTaskDB(t, f),
		loggerSet:  &loggers.LoggerSet{DB: quietLogger()},
		dedupCache: newDedupCache(0),
	}
	e.loadActiveTasks()

	if e.dedupCache.Len() != 0 {
		t.Fatalf("没有进行中的任务时不该有条目，实得 %d", e.dedupCache.Len())
	}
}

// 装载失败（库抖动）只记日志、不中断启动：去重退化成"每次都查库"，
// 由唯一索引兜底 —— 总好过整个爬虫起不来。
func TestLoadActiveTasksSurvivesDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	e := &Engine{
		db:         openFakeTaskDB(t, f),
		loggerSet:  &loggers.LoggerSet{DB: quietLogger()},
		dedupCache: newDedupCache(0),
	}
	e.loadActiveTasks() // 不该 panic

	if e.dedupCache.Len() != 0 {
		t.Fatalf("查询失败时缓存应保持空，实得 %d", e.dedupCache.Len())
	}
}

/* ---------- 工具 ---------- */

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !containsRaw(s, sub) {
			return false
		}
	}
	return true
}

func containsRaw(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// crawlerTaskStub 是 Upsert 的最小载体：只需要 gorm 认得出的字段与主键。
type crawlerTaskStub struct {
	ID    uint   `gorm:"primarykey"`
	URL   string `gorm:"type:varchar(500);uniqueIndex:idx_url_stage,priority:1"`
	Stage string `gorm:"type:varchar(50);uniqueIndex:idx_url_stage,priority:2"`
	Title string `gorm:"type:text"`
}

func (crawlerTaskStub) TableName() string { return "crawler_tasks" }

// 没启用静态 HTML 客户端（crawler.html.enable = false）时直接调 FetchHTML：给一句能照着改的错误，
// 而不是 nil 解引用 panic —— 框架 handler 里有 recover，会被吃成一次莫名其妙的任务失败；
// 直接调 API 的业务（cron / 后台任务）就是整个进程被带走。
func TestFetchHTMLWithoutClientReturnsError(t *testing.T) {
	e := &Engine{} // 没调 SetHTMLClient 的样子
	_, err := e.FetchHTML(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("未启用静态 HTML 客户端时应当返回错误")
	}
	if !strings.Contains(err.Error(), "html.enable") {
		t.Fatalf("报错要点名开关，实得：%v", err)
	}
}

// reap 与新池建之间没有锁：旧池被 reapIdle 摘掉后、reaper 调 cancelPoolErrorForwarder 之前，
// 同阶段可能已被重新提交、建了新池新转发协程。取消必须核对「归属池」，只按 stage key 关通道
// 会把新池的转发协程一起关掉，让新池的错误再也进不了 Errors()。
func TestCancelPoolErrorForwarderChecksPoolIdentity(t *testing.T) {
	e := &Engine{
		ctx:                context.Background(),
		loggerSet:          testLoggerSet(),
		errorCh:            make(chan error, 100),
		errorForwardCancel: make(map[string]*poolForwarder),
	}

	oldPool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e.startPoolErrorForwarder("stub", oldPool)

	// 同阶段换池：新转发协程占住同一个 key（旧池的转发协程随之退掉）
	newPool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e.startPoolErrorForwarder("stub", newPool)

	// 旧 reaper 只该停旧池的转发，不能误关新池的
	e.cancelPoolErrorForwarder("stub", oldPool)

	// 新池的转发协程还活着：停掉 newPool 触发错误，应该还能从 Errors() 收到
	newPool.Stop(0)
	_ = newPool.Submit(&Task{Stage: "stub", URL: "https://example.com"})

	select {
	case err := <-e.errorCh:
		if !strings.Contains(err.Error(), "already stopped") {
			t.Fatalf("应转发新池错误，实得：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("新池的转发协程被误关了：1s 内没收到错误")
	}
}
