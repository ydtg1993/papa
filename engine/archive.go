package engine

import (
	"context"
	"github.com/sirupsen/logrus"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// 本文件是**页面归档**：把「失败那一刻抓到的那一页」原样落到本地文件。
//
// 为什么是文件、而不是往 content 列塞：**原始字节**才是证据 —— goquery 重新序列化一遍会丢掉
// `<noscript>` 里的回退标签（延迟渲染的封面正藏在里面），而那恰恰是选择器坏掉时最该看的东西；
// 文件还能直接用浏览器打开、看它到底长什么样。
//
// 谁写盘、什么时候写：
//   - **登记**由 `FetchHTML` 自动做（handler 不用写任何代码）——页面成功拿到就登记进本次尝试的缓冲；
//   - **落盘**由 runAttempt 在尝试结束时按结局决定：`failure`（默认）只在失败的尝试落，
//     `always` 每次尝试都落。写盘失败只记 WARN，绝不影响任务本身 —— 归档是排查辅助，
//     不是任务的一部分。
//
// 两条抓取路径都覆盖：`FetchHTML`（静态）与 `FetchRendered`（浏览器渲染）——后者的原始 HTML
// 本来就是 `page.HTML()` 读出来的（解析文档必须付的代价），登记不额外花 CDP 调用。
// 渲染路径拿不到状态码与 Content-Type（信息在浏览器那边），归档的元信息里留零值。

const (
	// archiveCleanupInterval 归档保留期的巡检间隔（与 trace 的清理同频）。
	archiveCleanupInterval = time.Hour
	// defaultArchiveRetention 未配置保留期时的默认值（与 trace 对齐）。
	defaultArchiveRetention = 7 * 24 * time.Hour
	// defaultArchiveMaxBytes 未配置单页上限时的默认值。
	defaultArchiveMaxBytes = 8 << 20
	// maxArchivePagesPerAttempt 一次尝试最多登记几页。
	//
	// 正常 handler 一个尝试抓 1~2 页（列表 / 详情 / 播放页），全留在内存里没关系（HTML 本来就在
	// 内存里，登记不额外占）。但 handler 若是循环抓取，没有上界就等于把内存交给业务代码 ——
	// 超出的页面丢掉并记一条 WARN。
	maxArchivePagesPerAttempt = 32
	// archiveTraceStep 归档后写进 trace 的步骤名（OA 追踪抽屉里"失败"与"那一页"的连接点）。
	archiveTraceStep = "归档页面"
)

// archiveCtxKey 把本次尝试的归档缓冲挂到 ctx 上 —— `FetchHTML` 只拿到 ctx，
// 这是它唯一能知道"这一页属于哪次尝试"的途径（与 htmlfetch.WithProxy 同一个手法）。
type archiveCtxKey struct{}

// withArchive 把缓冲挂上 ctx；缓冲为 nil（未开归档）时原样返回，不白建一层。
func withArchive(ctx context.Context, ar *archiveBuffer) context.Context {
	if ar == nil {
		return ctx
	}
	return context.WithValue(ctx, archiveCtxKey{}, ar)
}

// archiveFromCtx 取出本次尝试的归档缓冲；没有（未开归档 / 不是从 worker 里调的）时返回 nil，
// 所有方法都对 nil 安全 —— 可选协作者就该是可选的。
func archiveFromCtx(ctx context.Context) *archiveBuffer {
	if ar, ok := ctx.Value(archiveCtxKey{}).(*archiveBuffer); ok {
		return ar
	}
	return nil
}

// archivedPage 归档一页所需的最小信息。
//
// 两条抓取路径都归到它：`FetchHTML`（静态，有状态码与 Content-Type）与 `FetchRendered`
// （浏览器渲染，**没有**状态码与 Content-Type —— 那些在 CDP 那边拿不到，留零值）。
type archivedPage struct {
	html        string
	url         string
	status      int
	contentType string
}

// archiveBuffer 一次尝试的归档缓冲：先登记，尝试结束时一次性落盘。
type archiveBuffer struct {
	store       archiveService
	logger      *logrus.Logger
	dir, mode   string
	maxBytes    int64
	task        *Task
	attempt     int
	executionID string

	pages   []archivedPage
	byURL   map[string]int // 最终 URL → pages 下标（同一个 URL 抓了两次时后来者覆盖）
	dropped int            // 超出上界 / 超过单页上限被丢掉的页数（只用于日志，见 finish）
}

// newArchiveBuffer 为一次尝试建缓冲；归档没开时返回 nil（调用方不用判空）。
func (e *Engine) newArchiveBuffer(task *Task, attempt int) *archiveBuffer {
	return e.archiveService().newBuffer(task, attempt)
}

// hold 登记静态抓取（`FetchHTML`）拿到的一页。只登记，不写盘；nil 安全。
func (a *archiveBuffer) hold(page *htmlfetch.Page) {
	if page == nil {
		return
	}
	a.holdPage(archivedPage{
		html:        page.HTML,
		url:         page.URL.String(),
		status:      page.StatusCode,
		contentType: page.ContentType,
	})
}

// holdRendered 登记浏览器渲染（`FetchRendered`）拿到的一页。
//
// 渲染路径的原始 HTML 是 `page.HTML()` 读出来的 —— 那次 CDP 调用**本来就要付**
// （`FetchRendered` 拿它去解析文档），所以这里登记不额外花什么。
// 状态码与 Content-Type 拿不到（信息在浏览器那边），留零值。
func (a *archiveBuffer) holdRendered(html, finalURL string) {
	a.holdPage(archivedPage{html: html, url: finalURL})
}

// holdPage 登记一页。nil 安全：未开归档时调用方拿到的就是 nil。
func (a *archiveBuffer) holdPage(p archivedPage) {
	if a == nil {
		return
	}
	// 单页上限在**登记时**判：既是"太大不归档"的判据，也顺带把内存兜住了
	if int64(len(p.html)) > a.maxBytes {
		a.dropped++
		a.logger.Warnf("archive: 页面超过单页上限，未归档（stage=%s task=%d url=%s bytes=%d）",
			a.task.Stage, a.task.ID, p.url, len(p.html))
		return
	}
	if idx, ok := a.byURL[p.url]; ok {
		// 同一个 URL 在这次尝试里抓了两次：留最后那一次（handler 重抓通常是因为前一次不可用）
		a.pages[idx] = p
		return
	}
	if len(a.pages) >= maxArchivePagesPerAttempt {
		a.dropped++
		return
	}
	a.byURL[p.url] = len(a.pages)
	a.pages = append(a.pages, p)
}

// finish 按结局落盘，返回写出去的相对路径（相对于归档根目录）。nil 安全。
//
// failed 由调用方给：handler 返回了 error、或 panic 展开（那种情况 err 还没被赋值，
// 所以 panic 也要算失败 —— 与 trace 那边"没走到 setResult 就按失败处理"同一取向）。
func (a *archiveBuffer) finish(failed bool) []string {
	if a == nil {
		return nil
	}
	if len(a.pages) == 0 {
		return nil
	}
	if a.mode != config.ArchiveModeAlways && !failed {
		return nil // failure 模式：成功的尝试不落盘（写入量按失败率走）
	}

	root := a.dir
	files := make([]string, 0, len(a.pages))
	for _, page := range a.pages {
		rel, err := a.store.writeOne(a.task, a.attempt, a.executionID, page)
		if err != nil {
			a.logger.Warnf("archive: 写盘失败（不影响任务）：%s", err.Error())
			continue
		}
		files = append(files, rel)
	}
	if len(files) > 0 || a.dropped > 0 {
		// dropped 要报出来：文件数少于"这一轮明明抓到了几页"时，得有个理由可查
		a.logger.Infof("archive: 已归档 %d 页、丢弃 %d 页（stage=%s task=%d try=%d）→ %s",
			len(files), a.dropped, a.task.Stage, a.task.ID, a.task.Retry, root)
	}
	return files
}

// sanitizePathSegment 清洗作为**目录名**用的站点名 / 阶段名：两者都来自业务代码，
// 但不该能让归档写到目录之外。空串落成 `_` —— 写盘这一步不做站点校验（旧行、手工构造的 Task
// 都可能带着空站点进来），落进一个固定名的目录比落到根目录好。
func sanitizePathSegment(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' ||
			r == '<' || r == '>' || r == '|' || r == '.' || r == 0:
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}

// Each facade operation snapshots current values, including hand-built Engine configurations.
func (e *Engine) archiveService() archiveService {
	service := archiveService{options: e.cfg.Crawler.Archive}
	if e.loggerSet != nil {
		service.logger = e.loggerSet.Engine
	}
	return service
}
func (e *Engine) archiveEnabled() bool {
	return (archiveService{options: e.cfg.Crawler.Archive}).enabled()
}
func (e *Engine) archiveDir() string { return e.cfg.Crawler.Archive.Dir }

// ArchiveDir returns the root directory or an empty string when disabled.
func (e *Engine) ArchiveDir() string {
	if !e.archiveEnabled() {
		return ""
	}
	return e.archiveDir()
}
func (e *Engine) archiveMode() string { return (archiveService{options: e.cfg.Crawler.Archive}).mode() }
func (e *Engine) archiveRetention() time.Duration {
	return (archiveService{options: e.cfg.Crawler.Archive}).retention()
}
func (e *Engine) archiveMaxBytes() int64 {
	return (archiveService{options: e.cfg.Crawler.Archive}).maxBytes()
}
func (e *Engine) startArchiveCleanup() {
	if !e.archiveEnabled() {
		return
	}
	retention := e.archiveRetention()
	if retention <= 0 {
		e.loggerSet.Engine.Infof("archive cleanup disabled: retention is negative (keep forever)")
		return
	}
	startEvidenceMaintenance(e.ctx, &e.tickerWG, archiveCleanupInterval, func(ctx context.Context) { e.archiveService().cleanup(ctx, retention) })
}
func (e *Engine) cleanupArchive(retention time.Duration) int {
	return e.archiveService().cleanup(e.ctx, retention)
}
