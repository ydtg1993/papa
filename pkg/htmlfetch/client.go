package htmlfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
)

const (
	defaultTimeout     = 15 * time.Second
	defaultMaxBodySize = 10 << 20
	defaultUserAgent   = "PapaStaticHTML/1.0"
)

// proxyModeKey 标记单次请求的代理模式
type proxyModeKey struct{}

// WithProxy 指定本次请求是否走代理：true 走代理，false 强制直连。
// 未设置时默认行为：配置了代理管理器则走代理，否则直连。
func WithProxy(ctx context.Context, use bool) context.Context {
	return context.WithValue(ctx, proxyModeKey{}, use)
}

// proxyFunc 返回 http.Transport.Proxy 使用的函数，按请求上下文决定是否走代理
func proxyFunc(manager *proxy.Manager) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
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
	httpClient *http.Client
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

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyFunc(cfg.ProxyManager)

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
		config: cfg,
	}
}

// SetProxyManager changes the proxy source used by subsequent requests.
func (c *Client) SetProxyManager(manager *proxy.Manager) {
	c.config.ProxyManager = manager
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		return
	}
	transport.Proxy = proxyFunc(manager)
}

// Fetch downloads and parses one static HTML page.
func (c *Client) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("url is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create html request: %w", err)
	}
	req.Header.Set("User-Agent", c.config.UserAgent)
	for key, value := range c.config.Headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch html: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch html: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.config.MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read html response: %w", err)
	}
	if int64(len(body)) > c.config.MaxBodySize {
		return nil, fmt.Errorf("html response exceeds %d bytes", c.config.MaxBodySize)
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
