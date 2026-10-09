package engine

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// splitHeaderOverride 把逐请求头拆成"要设的"与"要删的"（空值 = 删）——两条抓取路径共用这套语义。
func splitHeaderOverride(h map[string]string) (set map[string]string, remove []string) {
	if len(h) == 0 {
		return nil, nil
	}
	set = make(map[string]string, len(h))
	for k, v := range h {
		if v == "" {
			remove = append(remove, k)
			continue
		}
		set[k] = v
	}
	return set, remove
}

// defaultWaitTimeout 等待业务容器出现的默认超时。
const defaultWaitTimeout = 30 * time.Second

// FetchRendered 借浏览器渲染页面，等待 waitSelector（可选）出现后，
// 返回解析后的 goquery.Document 与导航后的最终 URL。用后自动归还浏览器。
func (e *Engine) FetchRendered(ctx context.Context, rawURL, waitSelector string) (*goquery.Document, string, error) {
	// 显式代理地址在浏览器路径上是**做不到**的：代理是浏览器实例级设置（启动参数），
	// Chrome/CDP 不提供逐请求改路由。静默忽略会让请求从别的出口出去、而调用方以为走了代理 ——
	// 所以在这里明确报错，并指出两条替代路。
	if addr := core.ProxyURLFrom(ctx); addr != "" {
		return nil, "", fmt.Errorf("浏览器路径不支持逐请求代理地址（%s）：代理是浏览器实例级设置，"+
			"Chrome 不提供逐请求改路由。要给这个站换出口，用 pool 上的代理配置、或一站一进程", addr)
	}

	pool := e.browserPool
	if pool == nil {
		return nil, "", fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	bw, err := pool.Get(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("acquire browser: %w", err)
	}
	defer pool.Put(bw)

	// 逐请求头（站点级 + `WithHeaders` 两层）走 PageOptions —— 浏览器那边本来就支持
	// "额外请求头合并默认头"，只是以前没人传；它是**导航前**设好的，首个请求就带上。
	// 空值表示删掉那个头，得单独交给 RemoveHeaders（rod 的默认头在内部先合并，光给空值删不掉）。
	reqHeaders, removeHeaders := splitHeaderOverride(core.HeadersFrom(ctx))
	page, err := bw.NewPageWithOptions(ctx, rawURL, browser.PageOptions{
		Headers:       reqHeaders,
		RemoveHeaders: removeHeaders,
	})
	if err != nil {
		return nil, "", fmt.Errorf("open page %s: %w", rawURL, err)
	}
	defer page.Close()

	if waitSelector != "" {
		if _, err := page.Timeout(defaultWaitTimeout).Element(waitSelector); err != nil {
			return nil, "", fmt.Errorf("wait selector %q on %s: %w", waitSelector, rawURL, err)
		}
	}

	html, err := page.HTML()
	if err != nil {
		return nil, "", fmt.Errorf("read html of %s: %w", rawURL, err)
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
		return nil, "", fmt.Errorf("get page info of %s: %w", rawURL, infoErr)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, "", fmt.Errorf("parse html of %s: %w", rawURL, err)
	}
	return doc, info.URL, nil
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
		BrowserPath:    e.cfg.Browser.BrowserPath,
		Flags:          map[string]string{},
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

// browserHeaders 合并内置默认头、基础配置、运行期覆盖，返回生效的浏览器默认请求头（每次新建 map）。
func (e *Engine) browserHeaders() map[string]string {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.Browser.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.Browser.Headers)
	maps.Copy(headers, e.runtime.Load().Browser.Headers)
	return headers
}

// browserMaxIdle 返回生效的浏览器空闲回收阈值。
func (e *Engine) browserMaxIdle() time.Duration {
	rt := e.runtime.Load()
	if rt.Browser.MaxIdleTime != nil {
		return rt.Browser.MaxIdleTime.Duration
	}
	return e.cfg.Browser.MaxIdleTime
}

// htmlConfig 计算生效的静态 HTML 客户端配置（基础 + 运行期覆盖）。
func (e *Engine) htmlConfig() htmlfetch.Config {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.HTML.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.HTML.Headers)
	rt := e.runtime.Load()
	maps.Copy(headers, rt.HTML.Headers)

	userAgent := headers["User-Agent"]
	delete(headers, "User-Agent")

	timeout := e.cfg.HTML.Timeout
	maxBody := e.cfg.HTML.MaxBodySize
	if rt.HTML.Timeout != nil {
		timeout = rt.HTML.Timeout.Duration
	}
	if rt.HTML.MaxBodySize != nil {
		maxBody = *rt.HTML.MaxBodySize
	}

	return htmlfetch.Config{
		Timeout:      timeout,
		MaxBodySize:  maxBody,
		UserAgent:    userAgent,
		Headers:      headers,
		ProxyManager: e.GetProxy(),
	}
}
