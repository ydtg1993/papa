package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
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
	e       *Engine
	task    *Task
	attempt int

	pages   []archivedPage
	byURL   map[string]int // 最终 URL → pages 下标（同一个 URL 抓了两次时后来者覆盖）
	dropped int            // 超出上界 / 超过单页上限被丢掉的页数（只用于日志，见 finish）
}

// newArchiveBuffer 为一次尝试建缓冲；归档没开时返回 nil（调用方不用判空）。
func (e *Engine) newArchiveBuffer(task *Task, attempt int) *archiveBuffer {
	if !e.archiveEnabled() || task.ID == 0 {
		return nil
	}
	return &archiveBuffer{e: e, task: task, attempt: attempt, byURL: make(map[string]int)}
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
	if int64(len(p.html)) > a.e.archiveMaxBytes() {
		a.dropped++
		a.e.loggerSet.Engine.Warnf("archive: 页面超过单页上限，未归档（stage=%s task=%d url=%s bytes=%d）",
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
	if a == nil || len(a.pages) == 0 {
		return nil
	}
	if a.e.archiveMode() != config.ArchiveModeAlways && !failed {
		return nil // failure 模式：成功的尝试不落盘（写入量按失败率走）
	}

	root := a.e.archiveDir()
	files := make([]string, 0, len(a.pages))
	for _, page := range a.pages {
		rel, err := a.writeOne(root, page)
		if err != nil {
			a.e.loggerSet.Engine.Warnf("archive: 写盘失败（不影响任务）：%s", err.Error())
			continue
		}
		files = append(files, rel)
	}
	if len(files) > 0 || a.dropped > 0 {
		// dropped 要报出来：文件数少于"这一轮明明抓到了几页"时，得有个理由可查
		a.e.loggerSet.Engine.Infof("archive: 已归档 %d 页、丢弃 %d 页（stage=%s task=%d try=%d）→ %s",
			len(files), a.dropped, a.task.Stage, a.task.ID, a.task.Retry, root)
	}
	return files
}

// writeOne 写一页：正文按**原样字节**落 .html，旁边落一份 .json 记这次请求的元信息。
func (a *archiveBuffer) writeOne(root string, page archivedPage) (string, error) {
	rel := a.relPath(page)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	// 原样字节：page.HTML 就是 htmlfetch 读到的 body，不做任何再序列化
	if err := os.WriteFile(abs, []byte(page.html), 0o644); err != nil {
		return "", err
	}

	meta := map[string]any{
		"task_id":      a.task.ID,
		"stage":        a.task.Stage,
		"attempt":      a.attempt,
		"retry":        a.task.Retry,
		"task_url":     a.task.URL,
		"final_url":    page.url,
		"status":       page.status,
		"content_type": page.contentType,
		"bytes":        len(page.html),
		"archived_at":  time.Now().Format(time.RFC3339),
	}
	if b, err := marshalNoHTMLEscape(meta); err == nil {
		// 元信息写不进去不算失败：正文才是证据。文件名把 .html 换成 .json（与正文同名同目录）
		_ = os.WriteFile(filepath.Join(root, strings.TrimSuffix(rel, ".html")+".json"), b, 0o644)
	}
	return rel, nil
}

// relPath 归档文件的相对路径：`{stage}/task-{id}-try-{retry}-{urlhash8}.html`。
//
// 带 try（= task.Retry）是必须的：同一任务重试 3 次时后面那次会覆盖前面那次，
// 而"第一次是登录墙、第三次正常"恰好是排查的关键。urlhash8 保证一个尝试里的多页不打架。
func (a *archiveBuffer) relPath(page archivedPage) string {
	sum := sha256.Sum256([]byte(page.url))
	name := fmt.Sprintf("task-%d-try-%d-%s.html", a.task.ID, a.task.Retry, hex.EncodeToString(sum[:4]))
	return filepath.Join(sanitizePathSegment(a.task.Stage), name)
}

// sanitizePathSegment 清洗作为**目录名**用的阶段名：阶段名来自业务代码，但不该能让归档写到目录之外。
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

/* ---------- 配置取值（默认值集中在引擎侧，与 traceRetention 一致） ---------- */

// archiveEnabled 归档是否生效：开关打开**且**目录非空。
// 目录为空时按未开启处理 —— 校验层会拦住这种配置，但直接把 Engine 拼出来的代码绕得过去，
// 而空目录会拼成"文件系统根"，那种后果比"没归档"严重得多。
func (e *Engine) archiveEnabled() bool {
	return e.cfg.Crawler.Archive.Enabled && e.cfg.Crawler.Archive.Dir != ""
}

func (e *Engine) archiveDir() string { return e.cfg.Crawler.Archive.Dir }

// ArchiveDir 归档根目录；归档没开时返回空串。
//
// 给后台"点开那一页"用（`GET /api/task/page?file=…`）：框架只暴露目录，
// 路径解析与防穿越在读取端做（见 admin/server/monitor.go 的 resolveArchivedFile）。
func (e *Engine) ArchiveDir() string {
	if !e.archiveEnabled() {
		return ""
	}
	return e.archiveDir()
}

// archiveMode 落盘时机；未配置按 failure。
func (e *Engine) archiveMode() string {
	if e.cfg.Crawler.Archive.Mode == config.ArchiveModeAlways {
		return config.ArchiveModeAlways
	}
	return config.ArchiveModeFailure
}

// archiveRetention 保留期；<=0（未配置）用默认，显式负数表示不自动清理。
func (e *Engine) archiveRetention() time.Duration {
	r := e.cfg.Crawler.Archive.Retention
	if r < 0 {
		return 0
	}
	if r == 0 {
		return defaultArchiveRetention
	}
	return r
}

// archiveMaxBytes 单页上限（字节）；未配置用默认。
func (e *Engine) archiveMaxBytes() int64 {
	if mb := e.cfg.Crawler.Archive.MaxFileMB; mb > 0 {
		return int64(mb) << 20
	}
	return defaultArchiveMaxBytes
}

/* ---------- 保留期清理 ---------- */

// startArchiveCleanup 启动归档文件的保留期清理（每小时巡检一次，与 trace 的清理同频）。
// 归档没开、或保留期为负（显式要求永久保留）时不启动。
func (e *Engine) startArchiveCleanup() {
	if !e.archiveEnabled() {
		return
	}
	retention := e.archiveRetention()
	if retention <= 0 {
		e.loggerSet.Engine.Infof("archive cleanup disabled: retention is negative (keep forever)")
		return
	}
	go func() {
		ticker := time.NewTicker(archiveCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.cleanupArchive(retention)
			}
		}
	}()
}

// cleanupArchive 删掉归档根目录下超过保留期的文件，返回删除数量。
//
// 按文件的修改时间判（归档文件写完就不再改）。单个条目读不到就跳过 —— 清理是后台杂活，
// 不能因为一个坏权限的目录把整轮中断；ctx 取消时立刻收工（Engine.Stop 会等这个协程）。
func (e *Engine) cleanupArchive(retention time.Duration) int {
	root := e.archiveDir()
	cutoff := time.Now().Add(-retention)
	var files int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-e.ctx.Done():
			return fs.SkipAll
		default:
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			return nil
		}
		if rerr := os.Remove(path); rerr != nil {
			_ = rerr // 删不掉就留到下一轮（可能正被人打开着）
			return nil
		}
		files++
		return nil
	})
	if err != nil {
		e.loggerSet.Engine.Warnf("archive cleanup: %s", err.Error())
	}
	if files > 0 {
		e.loggerSet.Engine.Infof("archive cleanup: 删除 %d 个超过保留期（%s）的归档文件", files, retention)
	}
	return files
}
