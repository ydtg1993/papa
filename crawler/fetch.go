package crawler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
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
