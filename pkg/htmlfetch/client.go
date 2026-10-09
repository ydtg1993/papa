package htmlfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
)

const (
	defaultTimeout     = 15 * time.Second
	defaultMaxBodySize = 10 << 20
	defaultUserAgent   = "PapaStaticHTML/1.0"
)

// proxyModeKey 标记单次请求的代理模式
type proxyModeKey struct{}

// WithProxy 指定本次请求是否走代理：true 走代理池，false 强制直连。
// 未设置时默认行为：配置了代理管理器则走代理，否则直连。
//
// 要指定**具体哪个**出口，用 `core.WithProxyURL(ctx, addr)`（它比这个开关更具体，优先于它）。
func WithProxy(ctx context.Context, use bool) context.Context {
	return context.WithValue(ctx, proxyModeKey{}, use)
}

// proxyFunc 返回 http.Transport.Proxy 使用的函数，按请求上下文决定是否走代理
func proxyFunc(manager *proxy.Manager) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		// 显式指定的出口最优先（`core.WithProxyURL`）：比"用不用代理池"更具体
		if addr := core.ProxyURLFrom(req.Context()); addr != "" {
			proxyURL, err := url.Parse(addr)
			if err != nil {
				return nil, fmt.Errorf("parse proxy URL %q: %w", addr, err)
			}
			return proxyURL, nil
		}
		if use, ok := req.Context().Value(proxyModeKey{}).(bool); ok && !use {
			return nil, nil
		}
		if manager == nil {
			return nil, nil
		}
		proxyAddress := manager.Next()
		if proxyAddress == "" {
			return nil, nil
		}
		proxyURL, err := url.Parse(proxyAddress)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		return proxyURL, nil
	}
}

// Config controls static HTML requests.
type Config struct {
	Timeout      time.Duration
	MaxBodySize  int64
	UserAgent    string
	Headers      map[string]string
	ProxyManager *proxy.Manager
}

// Client fetches server-rendered HTML without creating browser instances.
type Client struct {
	mu         sync.RWMutex
	httpClient *http.Client // 不含 Timeout，超时用 context 逐请求控制，支持运行期热更
	config     Config
}

// Page contains the response metadata and parsed HTML document.
type Page struct {
	URL         *url.URL
	StatusCode  int
	ContentType string
	HTML        string
	Document    *Document
}

// Document provides CSS selector queries against a parsed HTML document.
type Document struct {
	selection *goquery.Document
}

// NewClient creates a static HTML client.
func NewClient(cfg Config) *Client {
	applyConfigDefaults(&cfg)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyFunc(cfg.ProxyManager)

	return &Client{
		httpClient: &http.Client{Transport: transport},
		config:     cfg,
	}
}

// applyConfigDefaults 为零值字段填充默认值。
func applyConfigDefaults(cfg *Config) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = defaultMaxBodySize
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Headers == nil {
		cfg.Headers = make(map[string]string)
	}
}

// SetConfig 运行期更新请求配置（timeout/max_body_size/user_agent/headers），用于 OA 后台热更。
// 传入的 Headers 归 Client 所有，调用方随后不应再修改该 map。
// 代理在 NewClient 时定死（transport.Proxy），不随 SetConfig 改变。
func (c *Client) SetConfig(cfg Config) {
	applyConfigDefaults(&cfg)
	c.mu.Lock()
	cfg.ProxyManager = c.config.ProxyManager
	c.config = cfg
	c.mu.Unlock()
}

// Fetch downloads and parses one static HTML page.
func (c *Client) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("url is empty")
	}

	c.mu.RLock()
	cfg := c.config
	c.mu.RUnlock()

	// 逐请求超时，支持 SetConfig 热更 timeout
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create html request: %w", err)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	for key, value := range cfg.Headers {
		req.Header.Set(key, value)
	}
	// 逐请求覆盖（`core.WithHeaders`，站点级 + 单次请求两层）：同键覆盖，空值表示删掉那个头。
	// 放在配置之后 —— 越靠近这次请求的越优先。
	for key, value := range core.HeadersFrom(ctx) {
		if value == "" {
			req.Header.Del(key)
			continue
		}
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch html: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &StatusError{Code: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read html response: %w", err)
	}
	if int64(len(body)) > cfg.MaxBodySize {
		return nil, &BodyTooLargeError{MaxBodySize: cfg.MaxBodySize}
	}

	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse html document: %w", err)
	}

	return &Page{
		URL:         resp.Request.URL,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		HTML:        string(body),
		Document:    &Document{selection: document},
	}, nil
}

// Find returns all nodes matching selector.
func (d *Document) Find(selector string) *goquery.Selection {
	return d.selection.Find(selector)
}

// Text returns the text of the first matching node.
func (d *Document) Text(selector string) (string, bool) {
	node := d.Find(selector).First()
	if node.Length() == 0 {
		return "", false
	}
	return strings.TrimSpace(node.Text()), true
}

// Texts returns the text of every matching node.
func (d *Document) Texts(selector string) []string {
	texts := make([]string, 0)
	d.Find(selector).Each(func(_ int, selection *goquery.Selection) {
		texts = append(texts, strings.TrimSpace(selection.Text()))
	})
	return texts
}

// Attr returns an attribute of the first matching node.
func (d *Document) Attr(selector, attribute string) (string, bool) {
	value, exists := d.Find(selector).First().Attr(attribute)
	return value, exists
}

// Attrs returns an attribute from every matching node.
func (d *Document) Attrs(selector, attribute string) []string {
	values := make([]string, 0)
	d.Find(selector).Each(func(_ int, selection *goquery.Selection) {
		if value, exists := selection.Attr(attribute); exists {
			values = append(values, value)
		}
	})
	return values
}

// HTML returns the inner HTML of the first matching node.
func (d *Document) HTML(selector string) (string, bool) {
	node := d.Find(selector).First()
	if node.Length() == 0 {
		return "", false
	}
	value, err := node.Html()
	if err != nil {
		return "", false
	}
	return value, true
}
