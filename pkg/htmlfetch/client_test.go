package htmlfetch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
)

func TestClientFetchAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Errorf("User-Agent = %q, want %q", got, "test-agent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>标题</h1><a class="item" href="/one">第一项</a><a class="item" href="/two">第二项</a><div class="content"><b>正文</b></div></body></html>`))
	}))
	defer server.Close()

	client := NewClient(Config{
		Timeout:   time.Second,
		UserAgent: "test-agent",
		Headers:   map[string]string{"X-Test": "ok"},
	})
	page, err := client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if page.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", page.StatusCode, http.StatusOK)
	}
	if page.Document == nil {
		t.Fatal("Document is nil")
	}
	if got, ok := page.Document.Text("h1"); !ok || got != "标题" {
		t.Fatalf("Text(h1) = %q, %t", got, ok)
	}
	if got := page.Document.Texts("a.item"); len(got) != 2 || got[0] != "第一项" || got[1] != "第二项" {
		t.Fatalf("Texts(a.item) = %#v", got)
	}
	if got, ok := page.Document.Attr("a.item", "href"); !ok || got != "/one" {
		t.Fatalf("Attr(a.item, href) = %q, %t", got, ok)
	}
	if got := page.Document.Attrs("a.item", "href"); len(got) != 2 || got[1] != "/two" {
		t.Fatalf("Attrs(a.item, href) = %#v", got)
	}
	if got, ok := page.Document.HTML(".content"); !ok || got != "<b>正文</b>" {
		t.Fatalf("HTML(.content) = %q, %t", got, ok)
	}
}

func TestClientRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("123456"))
	}))
	defer server.Close()

	client := NewClient(Config{MaxBodySize: 5})
	_, err := client.Fetch(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds 5 bytes") {
		t.Fatalf("Fetch() error = %v, want response size error", err)
	}
}

func TestClientUsesProxyManager(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("target should only be reached through the proxy")
	}))
	defer target.Close()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() == false {
			t.Errorf("proxy request URL is not absolute: %s", r.URL.String())
		}
		if strings.TrimRight(r.URL.String(), "/") != strings.TrimRight(target.URL, "/") {
			t.Errorf("proxy request URL = %q, want %q", r.URL.String(), target.URL)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><p>through proxy</p></body></html>`))
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{
			"host": proxyURL.Hostname(),
			"port": proxyURL.Port(),
		}})
	}))
	defer proxyAPI.Close()

	proxyManager := proxy.NewManager(proxyAPI.URL, time.Minute)
	client := NewClient(Config{ProxyManager: proxyManager})

	page, err := client.Fetch(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Fetch() through proxy error = %v", err)
	}
	if got, ok := page.Document.Text("p"); !ok || got != "through proxy" {
		t.Fatalf("Text(p) = %q, %t", got, ok)
	}
}

func TestClientPerRequestProxyToggle(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><p>direct</p></body></html>`))
	}))
	defer target.Close()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><p>through-proxy</p></body></html>`))
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{
			"host": proxyURL.Hostname(),
			"port": proxyURL.Port(),
		}})
	}))
	defer proxyAPI.Close()

	proxyManager := proxy.NewManager(proxyAPI.URL, time.Minute)
	client := NewClient(Config{ProxyManager: proxyManager})

	// 默认：配置了代理则走代理
	page, err := client.Fetch(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("Fetch() default error = %v", err)
	}
	if got, ok := page.Document.Text("p"); !ok || got != "through-proxy" {
		t.Fatalf("default Text(p) = %q, %t, want through-proxy", got, ok)
	}

	// 显式直连
	page, err = client.Fetch(WithProxy(context.Background(), false), target.URL)
	if err != nil {
		t.Fatalf("Fetch() direct error = %v", err)
	}
	if got, ok := page.Document.Text("p"); !ok || got != "direct" {
		t.Fatalf("direct Text(p) = %q, %t, want direct", got, ok)
	}
}

func TestClientSetConfigHotReload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(r.Header.Get("User-Agent") + "|" + r.Header.Get("X-Test")))
	}))
	defer server.Close()

	client := NewClient(Config{
		Timeout:   time.Second,
		UserAgent: "agent-1",
		Headers:   map[string]string{"X-Test": "v1"},
	})
	page, err := client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if page.HTML != "agent-1|v1" {
		t.Fatalf("initial body = %q, want agent-1|v1", page.HTML)
	}

	client.SetConfig(Config{
		Timeout:   time.Second,
		UserAgent: "agent-2",
		Headers:   map[string]string{"X-Test": "v2"},
	})
	page, err = client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if page.HTML != "agent-2|v2" {
		t.Fatalf("after SetConfig body = %q, want agent-2|v2", page.HTML)
	}
}

// 非 2xx 要有类型，业务才不用去匹配错误文本（文本匹配会把 4040000 这种数字读成 404）。
// 文案**保持不变** —— 这次只是多给一条路，不逼着已经写了匹配的代码立刻改。
func TestFetchStatusErrorIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(Config{Timeout: time.Second})
	_, err := client.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("404 应当报错")
	}

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("应能取出 *StatusError，实得 %T：%v", err, err)
	}
	if se.Code != http.StatusNotFound {
		t.Fatalf("Code = %d, want 404", se.Code)
	}
	if code, ok := StatusCode(err); !ok || code != http.StatusNotFound {
		t.Fatalf("StatusCode = (%d, %t)，want (404, true)", code, ok)
	}
	if err.Error() != "fetch html: unexpected status 404" {
		t.Fatalf("文案变了（老代码可能依赖它）：%q", err.Error())
	}
}

// 响应体超限是**另一个**类型，不是状态码错误 —— 这条用例钉的正是"文本匹配会误伤"那个场景：
// max_body_size 配成 404 时，报错文本里就带着 "404"，按 strings.Contains 判 not_found 会误判。
func TestBodyTooLargeIsNotAStatusError(t *testing.T) {
	body := strings.Repeat("x", 405)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewClient(Config{MaxBodySize: 404})
	_, err := client.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("超出 max_body_size 应当报错")
	}

	if code, ok := StatusCode(err); ok {
		t.Fatalf("响应体超限不是状态码错误，却取出了 %d", code)
	}
	var tooLarge *BodyTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.MaxBodySize != 404 {
		t.Fatalf("应能取出 *BodyTooLargeError/Max=404，实得 %T：%v", err, err)
	}
	// 这条用例的前提：报错文本里确实带着 "404"（所以文本匹配才会误伤）
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("前提不成立，文本里应含 404：%q", err.Error())
	}
	// 文案同样保持原样
	if err.Error() != "html response exceeds 404 bytes" {
		t.Fatalf("文案变了：%q", err.Error())
	}
}
