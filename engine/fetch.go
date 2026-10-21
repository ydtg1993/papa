package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/textproto"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// splitHeaderOverride 把逐请求头拆成"要设的"与"要删的"（空值 = 删）——两条抓取路径共用这套语义。
//
// 键按 HTTP 语义规范化：header 名大小写不敏感，站点级可能是 `Authorization`、逐请求覆盖/删除
// 写的却是 `authorization`—— 不规范化的话，浏览器路径（map 精确键删）会删不掉、旧凭据照发。
// 静态路径（net/http 的 Set/Del）本身会 canonical 化，但这里统一规范化，消费方不用各做各的。
func splitHeaderOverride(h map[string]string) (set map[string]string, remove []string) {
	if len(h) == 0 {
		return nil, nil
	}
	set = make(map[string]string, len(h))
	for k, v := range h {
		k = textproto.CanonicalMIMEHeaderKey(k)
		if v == "" {
			remove = append(remove, k)
			continue
		}
		set[k] = v
	}
	return set, remove
}

// renderWaitTraceStep 渲染等待失败在 trace 里的步骤名。
const renderWaitTraceStep = "等待渲染"

// FetchRendered 借浏览器渲染页面，等待 waitSelector（可选）出现后，
// 返回解析后的 goquery.Document 与导航后的最终 URL。用后自动归还浏览器。
//
// 要 `*htmlfetch.Page` —— 好直接喂给 `engine.RestrictedError`（受限页判据）或 archive 那套
// 收 *Page 的工具 —— 用 FetchRenderedPage：两者抓的是同一份 HTML，只是包成什么给调用方不同。
func (e *Engine) FetchRendered(ctx context.Context, rawURL, waitSelector string) (*goquery.Document, string, error) {
	html, finalURL, err := e.render(ctx, rawURL, browser.PageOptions{WaitSelector: waitSelector})
	if err != nil {
		return nil, "", err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, "", fmt.Errorf("parse html of %s: %w", rawURL, err)
	}
	return doc, finalURL, nil
}

// FetchRenderedPage 借浏览器渲染页面，返回 `*htmlfetch.Page` —— 与 `FetchHTML` 同一个类型。
//
// 为什么要有它：`FetchHTML` 拿到的页能直接喂给 `engine.RestrictedError`（受限页判据）与 archive，
// 因为那两处收的都是 `*htmlfetch.Page`；而 `FetchRendered` 只给 `*goquery.Document`，
// 于是**走浏览器的阶段用不上框架的受限页判据** —— 被反爬拦下时只表现为"解析不出东西"，
// 与静态路径的 `access_restricted`（不可重试、消息里带上命中的那句话）差着一层。
//
// 拿到的 Page 与静态路径的差别：StatusCode / ContentType 留零值（浏览器不给，见
// htmlfetch.NewRenderedPage）；归档与 FetchRendered 完全一致（登记的本来就是渲染后的 HTML）。
//
//	page, err := engine.FetchRenderedPage(ctx, task.URL, ".detail-box")
//	if err != nil { return err }
//	if err := engine.RestrictedError(task, page, ""); err != nil { return err }
func (e *Engine) FetchRenderedPage(ctx context.Context, rawURL, waitSelector string) (*htmlfetch.Page, error) {
	return e.FetchRenderedPageWith(ctx, rawURL, browser.PageOptions{WaitSelector: waitSelector})
}

// FetchRenderedPageWith 与 FetchRenderedPage 相同，但把页面准备与等待条件全部交给
// browser.PageOptions：设备、请求头、Cookies、导航超时，以及 WaitSelector / WaitJS /
// WaitRequestIdle（顺序固定，共享 Timeout 的等待预算）。
//
// ctx 上的站点级 / 逐请求头仍会合并进来；显式 opts.Headers 优先。
// 等待失败时页面在关闭前被读一次 HTML，随 browser.WaitError 带出 —— 这里会把它登记进
// 归档并在 trace 写一条「等待渲染」失败步骤。
func (e *Engine) FetchRenderedPageWith(ctx context.Context, rawURL string, opts browser.PageOptions) (*htmlfetch.Page, error) {
	html, finalURL, err := e.render(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	page, err := htmlfetch.NewRenderedPage(finalURL, html)
	if err != nil {
		return nil, fmt.Errorf("parse html of %s: %w", rawURL, err)
	}
	return page, nil
}

// browserProxyError 报告浏览器路径不支持的「逐请求代理地址」；没配时返回 nil。
//
// 为什么不能静默忽略：代理是浏览器实例级设置（启动参数），Chrome/CDP 不提供逐请求改路由。
// 忽略会让请求从别的出口出去、而调用方以为走了代理 —— 所以两条浏览器抓取路径
// （render / CaptureRequest）都在入口处明确报错，并指出替代路。
func browserProxyError(ctx context.Context) error {
	if addr := core.ProxyURLFrom(ctx); addr != "" {
		return fmt.Errorf("浏览器路径不支持逐请求代理地址（%s）：代理是浏览器实例级设置，"+
			"Chrome 不提供逐请求改路由。要给这个站换出口，用 pool 上的代理配置、或一站一进程", addr)
	}
	return nil
}

// mergeBrowserOptions 把 ctx 上的站点级 / 逐请求头合并进 PageOptions；
// 显式 opts.Headers 是 API 调用方给的最后一层，优先。
//
// 逐请求头（站点级 + `WithHeaders` 两层）走 PageOptions —— 浏览器那边本来就支持
// "额外请求头合并默认头"；它是**导航前**设好的，首个请求就带上。
// 空值表示删掉那个头，得单独交给 RemoveHeaders（rod 的默认头在内部先合并，光给空值删不掉）。
func mergeBrowserOptions(ctx context.Context, opts browser.PageOptions) browser.PageOptions {
	reqHeaders, removeHeaders := splitHeaderOverride(core.HeadersFrom(ctx))
	if len(reqHeaders) > 0 {
		merged := make(map[string]string, len(reqHeaders)+len(opts.Headers))
		maps.Copy(merged, reqHeaders)
		maps.Copy(merged, opts.Headers)
		opts.Headers = merged
	}
	if len(removeHeaders) > 0 {
		opts.RemoveHeaders = append(opts.RemoveHeaders, removeHeaders...)
	}
	return opts
}

// recordRenderWaitFailure 把「导航成功、等待失败」的现场写进归档与 trace。
// 只处理 browser.WaitError：导航本身失败没有页面可看，走原来的错误路径。
func recordRenderWaitFailure(ctx context.Context, rawURL string, opts browser.PageOptions, err error) {
	var waitErr *browser.WaitError
	if !errors.As(err, &waitErr) {
		return
	}
	// 页面在 NewPageWithOptions 的失败路径上已经关了；HTML 是它关闭前读出来的现场。
	if waitErr.HTML != "" {
		archiveFromCtx(ctx).holdRendered(waitErr.HTML, rawURL)
	}
	data := map[string]any{
		"wait_stage": waitErr.Stage,
		"elapsed_ms": waitErr.Elapsed.Milliseconds(),
	}
	if opts.WaitSelector != "" {
		data["wait_selector"] = opts.WaitSelector
	}
	if opts.WaitJS != "" {
		data["wait_js"] = opts.WaitJS
	}
	if opts.WaitRequestIdle > 0 {
		data["wait_request_idle_ms"] = opts.WaitRequestIdle.Milliseconds()
	}
	traceFromCtx(ctx).Fail(renderWaitTraceStep, waitErr, data)
}

// render 借浏览器渲染一页，把原始 HTML 与导航后的最终 URL 交出去；两个公开方法共用它
// （差别只在"把这份 HTML 包成 Document 还是 Page"）。归档也在这里做：那份 HTML 本来就在手上。
//
// opts 里的 WaitSelector / WaitJS / WaitRequestIdle 由 NewPageWithOptions 应用；
// 等待失败时会带回 browser.WaitError（含现场 HTML），这里负责归档与 trace。
func (e *Engine) render(ctx context.Context, rawURL string, opts browser.PageOptions) (string, string, error) {
	if err := browserProxyError(ctx); err != nil {
		return "", "", err
	}

	pool := e.browserPool
	if pool == nil {
		return "", "", fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	bw, err := pool.Get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("acquire browser: %w", err)
	}
	defer pool.Put(bw)

	opts = mergeBrowserOptions(ctx, opts)
	page, err := bw.NewPageWithOptions(ctx, rawURL, opts)
	if err != nil {
		recordRenderWaitFailure(ctx, rawURL, opts, err)
		return "", "", fmt.Errorf("open page %s: %w", rawURL, err)
	}
	defer page.Close()

	html, err := page.HTML()
	if err != nil {
		return "", "", fmt.Errorf("read html of %s: %w", rawURL, err)
	}
	// 归档：这份 HTML 本来就在手上（上面那次 page.HTML() 是解析文档必须付的代价），
	// 顺手登记 —— 于是渲染路径的失败现场（选择器等错了、渲染没出来）也留得下。
	// 先取 info 是为了拿导航后的最终 URL 给文件命名；info 出错时退回请求的 URL，
	// **不能因此丢掉归档**（那正是最该看现场的时候）。
	info, infoErr := page.Info()
	finalURL := rawURL
	if infoErr == nil && info.URL != "" {
		finalURL = info.URL
	}
	archiveFromCtx(ctx).holdRendered(html, finalURL)
	if infoErr != nil {
		return "", "", fmt.Errorf("get page info of %s: %w", rawURL, infoErr)
	}

	// 返回值用 info.URL（导航后的地址），归档用的是上面的 finalURL（取不到 info 时退回请求的 URL）——
	// 与拆成两条公开方法之前逐字一致。
	return html, info.URL, nil
}

// defaultHeaders 浏览器与静态 HTML 客户端共用的默认请求头，配置中的 headers 会覆盖同名项
var defaultHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}

// SetBrowserPool 创建浏览器操作池，未启用时跳过
func (e *Engine) SetBrowserPool() {
	if !e.cfg.Browser.Enable {
		return
	}
	pool, err := browser.NewPool(browser.PoolConfig{
		Size:           e.cfg.Browser.PoolSize,
		DirectSize:     e.cfg.Browser.DirectSize,
		MaxIdleTime:    e.browserMaxIdle(),
		Headless:       e.cfg.Browser.Headless,
		NoSandbox:      e.cfg.Browser.NoSandbox,
		Leakless:       e.cfg.Browser.Leakless,
		BinarySource:   e.cfg.Browser.Binary,
		BrowserPath:    e.cfg.Browser.BrowserPath,
		Flags:          e.cfg.Browser.Flags,
		SelfCheck:      e.cfg.Browser.SelfCheck,
		DefaultHeaders: e.browserHeaders(),
		ProxyManager:   e.GetProxy(),
	})
	if err != nil {
		panic(fmt.Errorf("new browser pool: %s", err.Error()))
	}
	e.browserPool = pool
}

// GetBrowserPool 获取浏览器池
func (e *Engine) GetBrowserPool() *browser.Pool {
	return e.browserPool
}

// GetHTMLClient 获取静态 HTML 抓取客户端
func (e *Engine) GetHTMLClient() *htmlfetch.Client {
	return e.htmlClient
}

// NextProxy 取一个代理地址；没配代理管理器或池子为空时返回空串。
//
// 给"用哪个出口由 fetcher 自己决定"用：
//
//	if p := engine.NextProxy(); p != "" {
//	    page, err = engine.FetchHTML(papa.WithProxyURL(ctx, p), url)
//	}
//
// 它只是**取一个**；用不用、用几个、怎么轮换都由业务定 —— 与 GetFiledown() / GetM3U8()
// 那套"按需取"一致。
func (e *Engine) NextProxy() string {
	if m := e.proxy; m != nil {
		return m.Next()
	}
	return ""
}

// FetchHTML 抓取并解析静态 HTML 页面，不创建浏览器实例。
//
// 开了 `crawler.archive` 时，这里会**自动**把抓到的页面登记进本次尝试的归档缓冲
// （handler 不用写任何代码，见 archive.go）——登记不写盘，落盘在本次尝试结束时按结局决定。
func (e *Engine) FetchHTML(ctx context.Context, rawURL string) (*htmlfetch.Page, error) {
	// 未启用时 SetHTMLClient 不建客户端（cfg.HTML.Enable=false），直接解引用就是 nil panic ——
	// 给一句能照着改的错，别让业务去猜"为什么抓一下就崩"。
	if e.htmlClient == nil {
		return nil, fmt.Errorf("静态 HTML 客户端未启用（crawler.html.enable = false）：FetchHTML 用不了。" +
			"要用浏览器渲染走 FetchRendered（browser.enable），或把 html.enable 打开")
	}
	page, err := e.htmlClient.Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	archiveFromCtx(ctx).hold(page)
	return page, nil
}

// SetHTMLClient 创建静态 HTML 抓取客户端，未启用时跳过
func (e *Engine) SetHTMLClient() {
	if !e.cfg.HTML.Enable {
		return
	}
	e.htmlClient = htmlfetch.NewClient(e.htmlConfig())
}

// browserHeaders 合并内置默认头与配置里的浏览器默认头（每次新建 map）。
func (e *Engine) browserHeaders() map[string]string {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.Browser.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.Browser.Headers)
	return headers
}

// browserMaxIdle 返回浏览器空闲回收阈值。
func (e *Engine) browserMaxIdle() time.Duration { return e.cfg.Browser.MaxIdleTime }

// htmlConfig 计算静态 HTML 客户端的配置（内置默认头 + 配置里的头）。
func (e *Engine) htmlConfig() htmlfetch.Config {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.HTML.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.HTML.Headers)

	userAgent := headers["User-Agent"]
	delete(headers, "User-Agent")

	return htmlfetch.Config{
		Timeout:      e.cfg.HTML.Timeout,
		MaxBodySize:  e.cfg.HTML.MaxBodySize,
		UserAgent:    userAgent,
		Headers:      headers,
		ProxyManager: e.GetProxy(),
	}
}
