package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
	"github.com/ydtg1993/papa/v2/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v2/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
	"gorm.io/gorm"
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
	m3u := &m3u8.Downloader{}
	fd := filedown.NewDownloader(nil)
	e.SetProxy(pm)
	e.SetM3U8(m3u)
	e.SetFiledown(fd)

	if e.GetProxy() != pm {
		t.Fatal("SetProxy/GetProxy 不是同一个对象")
	}
	if e.GetM3U8() != m3u {
		t.Fatal("SetM3U8/GetM3U8 不是同一个对象")
	}
	if e.GetFiledown() != fd {
		t.Fatal("SetFiledown/GetFiledown 不是同一个对象")
	}
}

// Errors 按阶段给出各自的错误通道；业务把它们逐个丢给日志协程。
func TestEngineErrorsPerStage(t *testing.T) {
	f := newFakeTaskDB()
	poolA := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, poolA)
	e.stages["second"] = &stageInfo{workerPool: workerpool.NewWorkerPool[*Task](1, 8, 1)}

	chans := e.Errors()
	if len(chans) != 2 {
		t.Fatalf("两个阶段应给两条错误通道，实得 %d", len(chans))
	}
	poolA.Errors()
	for _, ch := range chans {
		if ch == nil {
			t.Fatal("错误通道不该为 nil")
		}
	}
}

/* ---------- 运行期配置热更 ---------- */

// ApplyRuntimeConfig 除了落覆盖层，还要给动态 ticker 发一次"配置变了"的信号 ——
// 否则改了队列 interval 得等下一次重启才生效。
func TestApplyRuntimeConfigSignalsTicker(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	interval := config.Duration{Duration: time.Minute}
	enabled := true
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		ErrorQueue: config.RuntimeErrorQueueConfig{Enabled: &enabled, Interval: &interval},
	}); err != nil {
		t.Fatalf("ApplyRuntimeConfig = %v", err)
	}

	select {
	case <-e.configChanged:
	default:
		t.Fatal("应当给 configChanged 发一次信号")
	}

	// 生效配置要真的读得到覆盖值
	got := e.errorQueueConfig()
	if !got.Enabled || got.Interval != time.Minute {
		t.Fatalf("生效配置 = %+v", got)
	}

	// 信号是"非阻塞提醒"，连着来两次不该把调用方卡住
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{}); err != nil {
		t.Fatalf("第二次 ApplyRuntimeConfig = %v", err)
	}
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{}); err != nil {
		t.Fatalf("第三次 ApplyRuntimeConfig 不该阻塞：%v", err)
	}
}

// 浏览器池 / HTML 客户端没建时，热更那两句要跳过而不是解引用 nil。
func TestApplyRuntimeConfigSkipsMissingComponents(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	if e.browserPool != nil || e.htmlClient != nil {
		t.Fatal("前置条件：组件都没建")
	}
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{}); err != nil {
		t.Fatalf("ApplyRuntimeConfig = %v", err)
	}
}

/* ---------- 结果读写 ---------- */

type resultPayload struct {
	N int    `json:"n"`
	S string `json:"s"`
}

// SaveResult 按列写 title + content，不整行 Save ——
// 整行写会把 worker 刚认领时写的 status 覆盖回去。
func TestSaveResultOnlyTouchesTitleAndContent(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	if err := e.SaveResult(7, "标题", resultPayload{N: 1, S: "x"}); err != nil {
		t.Fatalf("SaveResult = %v", err)
	}
	sql := f.written()
	if !containsAll(sql, "title", "content") {
		t.Fatalf("应更新 title 与 content：\n%s", sql)
	}
	for _, never := range []string{"`status`", "`retry`", "`url`"} {
		if containsAll(sql, never) {
			t.Fatalf("不该碰 %s：\n%s", never, sql)
		}
	}
	if !containsAll(f.writtenArgs(), "标题") {
		t.Fatalf("title 应作为参数写进去：%s", f.writtenArgs())
	}
}

func TestSaveContentKeepsTitle(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	if err := e.SaveContent(7, resultPayload{N: 2}); err != nil {
		t.Fatalf("SaveContent = %v", err)
	}
	sql := f.written()
	if !containsAll(sql, "content") {
		t.Fatalf("应更新 content：\n%s", sql)
	}
	if containsAll(sql, "`title`") {
		t.Fatalf("SaveContent 不该碰 title：\n%s", sql)
	}
}

// 序列化失败要报错且一条 SQL 都不发（否则会往 content 里写进半截数据）。
func TestSaveResultRejectsUnmarshalableContent(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	bad := make(chan int)
	if err := e.SaveResult(7, "t", bad); err == nil {
		t.Fatal("chan 不可序列化，应当报错")
	}
	if err := e.SaveContent(7, bad); err == nil {
		t.Fatal("chan 不可序列化，应当报错")
	}
	if got := f.written(); got != "" {
		t.Fatalf("序列化失败不该发 SQL：\n%s", got)
	}
}

func TestGetResultRoundTrip(t *testing.T) {
	f := newFakeTaskDB()
	f.row["content"] = []byte(`{"n":42,"s":"hi"}`)
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	var out resultPayload
	if err := e.GetResult(7, &out); err != nil {
		t.Fatalf("GetResult = %v", err)
	}
	if out.N != 42 || out.S != "hi" {
		t.Fatalf("反序列化结果 = %+v", out)
	}
}

// content 为空（列是 NULL 或空串）时不算错误：任务可能只是还没写结果。
// GetResult **不做初始化**：out 保持调用方传进来的样子。
func TestGetResultEmptyContentIsNotAnError(t *testing.T) {
	f := newFakeTaskDB()
	f.row["content"] = nil
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	out := resultPayload{N: 7}
	if err := e.GetResult(7, &out); err != nil {
		t.Fatalf("GetResult = %v", err)
	}
	if out.N != 7 {
		t.Fatalf("空 content 时不该动 out：%+v", out)
	}
}

func TestGetResultMissingRow(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	var out resultPayload
	err := e.GetResult(7, &out)
	if err == nil {
		t.Fatal("行不存在应当报错")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("应当把 gorm 的哨兵错误原样透出（业务据此判定没有这条任务），实得 %v", err)
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
	e.runtime.Store(&config.RuntimeConfig{})

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
	e.runtime.Store(&config.RuntimeConfig{})

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
	e.runtime.Store(&config.RuntimeConfig{})

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

	if !e.dedupCache.Get("stub|https://example.com/active") {
		t.Fatal("进行中的任务应被装进去重表")
	}
	// 装的是 pending/processing + repeatable，条件必须写进查询
	read := f.readSQL()
	if !containsAll(read, "status IN") || !containsAll(read, "repeatable") {
		t.Fatalf("装载条件不完整：\n%s", read)
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

/* ---------- 业务自定义指标 ---------- */

// 引擎没建注册表（测试里手搓的 Engine）时 RecordMetric 是安全的 no-op。
func TestRecordMetricNilRegistryIsSafe(t *testing.T) {
	e := &Engine{}
	e.RecordMetric("k", 1)

	got := e.GetMetrics()
	// 框架级计数仍在
	for _, k := range []string{"queue_spilled", "queue_spill_backlog", "recover_total", "error_retry_total", "repeat_repoll_total"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("GetMetrics 少了框架级计数 %q：%v", k, got)
		}
	}
	if _, ok := got["k"]; ok {
		t.Fatal("没有注册表时业务指标不该出现")
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

// content 列里不该出现 `<` 这类 HTML 转义。
//
// 业务存进 content 的常常就是一段 HTML（页面片段、页面快照、带 `<a>` 的富文本），
// 转义之后在库里、后台表格里、SQL 客户端里都是一堆 `<`，等于"存了但看不了一眼"。
// json.Unmarshal 把两种写法还原成同一个字符串，所以改这个**不影响已经落库的老数据**（见下）。
func TestSaveResultDoesNotHTMLEscapeContent(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	const snippet = `<div class="poster">a & b</div>`
	if err := e.SaveResult(7, "标题", map[string]string{"html": snippet}); err != nil {
		t.Fatalf("SaveResult = %v", err)
	}

	args := f.writtenArgs()
	// JSON 层该转义的照旧（双引号写成 \"，那是 JSON 本身的要求），这里只看 HTML 的那几个字符
	// 有没有被一起转掉 —— 所以用不带引号的片段来对，免得把 JSON 的引号转义也算进来。
	for _, raw := range []string{`<div class=`, `>a & b<`, `</div>`} {
		if !strings.Contains(args, raw) {
			t.Fatalf("落库的应是原样的片段，参数里缺少 %s：%s", raw, args)
		}
	}
	// 反面：默认的 json.Marshal 会把它们写成反斜杠转义（`backslash-u003c` 那种形态），那正是要避免的。
	// 拿它的输出来比，比在断言里手写转义序列可靠 —— 手写的转义序列很容易被各种工具还原成原文。
	if escaped, _ := json.Marshal(map[string]string{"html": snippet}); strings.Contains(args, string(escaped)) {
		t.Fatalf("content 落库的是 json.Marshal 的 HTML 转义形态：%s", args)
	}

	// 老数据是转义形态（旧版 SaveResult 留下的），照样读得回来 —— 转义只是 JSON 的一种写法，
	// 反序列化后是同一个字符串。这里直接用 json.Marshal 造出那份老数据。
	old, _ := json.Marshal(map[string]string{"html": "<div>"})
	f.row["content"] = old
	var back map[string]string
	if err := e.GetResult(7, &back); err != nil {
		t.Fatalf("GetResult = %v", err)
	}
	if back["html"] != "<div>" {
		t.Fatalf("转义形态应能原样还原，实得 %q", back["html"])
	}
}
