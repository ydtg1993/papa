package engine

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v2/pkg/browser"
	"github.com/ydtg1993/papa/v2/pkg/htmlfetch"
)

// defaultWaitTimeout 等待业务容器出现的默认超时。
const defaultWaitTimeout = 30 * time.Second

// FetchRendered 借浏览器渲染页面，等待 waitSelector（可选）出现后，
// 返回解析后的 goquery.Document 与导航后的最终 URL。用后自动归还浏览器。
func (e *Engine) FetchRendered(ctx context.Context, rawURL, waitSelector string) (*goquery.Document, string, error) {
	pool := e.browserPool
	if pool == nil {
		return nil, "", fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	browser, err := pool.Get(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("acquire browser: %w", err)
	}
	defer pool.Put(browser)

	page, err := browser.NewPage(ctx, rawURL)
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
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, "", fmt.Errorf("parse html of %s: %w", rawURL, err)
	}

	info, err := page.Info()
	if err != nil {
		return nil, "", fmt.Errorf("get page info of %s: %w", rawURL, err)
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

// FetchHTML 抓取并解析静态 HTML 页面，不创建浏览器实例
func (e *Engine) FetchHTML(ctx context.Context, rawURL string) (*htmlfetch.Page, error) {
	return e.htmlClient.Fetch(ctx, rawURL)
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
