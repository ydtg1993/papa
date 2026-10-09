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

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
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

// 逐请求头（`core.WithHeaders`）：同键覆盖配置里的、空值删掉配置里的，其余照旧。
func TestFetchAppliesPerRequestHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	defer server.Close()

	client := NewClient(Config{
		Timeout:   time.Second,
		UserAgent: "cfg-ua",
		Headers:   map[string]string{"X-From-Config": "1", "X-Gone": "yes", "X-Kept": "keep"},
	})
	ctx := core.WithHeaders(context.Background(), map[string]string{
		"User-Agent":    "req-ua",
		"X-From-Config": "2",
		"X-Gone":        "",
	})
	if _, err := client.Fetch(ctx, server.URL); err != nil {
		t.Fatalf("Fetch = %v", err)
	}

	if v := got.Get("User-Agent"); v != "req-ua" {
		t.Fatalf("User-Agent = %q，逐请求应当覆盖配置里的", v)
	}
	if v := got.Get("X-From-Config"); v != "2" {
		t.Fatalf("X-From-Config = %q，应当被逐请求覆盖", v)
	}
	if _, ok := got["X-Gone"]; ok {
		t.Fatalf("空值应当把配置里的头删掉，实得 %v", got)
	}
	if v := got.Get("X-Kept"); v != "keep" {
		t.Fatalf("没提到的头要留着，实得 %q", v)
	}
}

// 显式出口（`core.WithProxyURL`）优先于"用不用代理池"那个开关：出口由调用方决定。
func TestProxyURLOverridesManager(t *testing.T) {
	proxyFn := proxyFunc(nil) // nil manager = 没配代理池

	// 只有"强制直连"开关：返回 nil（直连）
	direct := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	direct = direct.WithContext(WithProxy(direct.Context(), false))
	if u, err := proxyFn(direct); err != nil || u != nil {
		t.Fatalf("直连应当返回 nil，实得 %v %v", u, err)
	}

	// 显式地址：即使开关说直连，也走指定的出口（它更具体）
	withURL := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	withURL = withURL.WithContext(core.WithProxyURL(WithProxy(withURL.Context(), false), "http://1.2.3.4:8080"))
	u, err := proxyFn(withURL)
	if err != nil {
		t.Fatalf("解析显式代理地址：%v", err)
	}
	if u == nil || u.String() != "http://1.2.3.4:8080" {
		t.Fatalf("应当用显式指定的出口，实得 %v", u)
	}

	// 地址写错：报错而不是静默直连
	bad := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	bad = bad.WithContext(core.WithProxyURL(bad.Context(), "://坏地址"))
	if _, err := proxyFn(bad); err == nil {
		t.Fatal("地址解析不出来时应当报错（静默直连会让请求从别的出口出去）")
	}
}

// 受限页判据：默认词表扫**可见正文 + 标题**，外加业务追加的本站文案。
func TestRestrictedReason(t *testing.T) {
	cases := []struct {
		name  string
		html  string
		extra []string
		want  string
	}{
		{"正文里的 captcha", `<html><body><p>Please complete the CAPTCHA to continue</p></body></html>`, nil, "captcha"},
		{"中文验证码", `<html><body><div>请填写验证码后继续</div></body></html>`, nil, "验证码"},
		{"只有标题命中（Cloudflare 拦截页正文很空）", `<html><head><title>Just a moment...</title></head><body></body></html>`, nil, "just a moment"},
		{"业务追加的本站文案", `<html><body>安全验证中，请稍候</body></html>`, []string{"安全验证"}, "安全验证"},
		{"正常页面不误判", `<html><body><h1>某剧集</h1><p>第 1 集</p></body></html>`, nil, ""},
		// 关键：**内联脚本**里有 captcha 字面量（埋点/第三方 SDK 常见），那不是页面对人说的话
		{"内联脚本里的 captcha 不算", `<html><body><script>var t="captcha";</script><p>正常正文</p></body></html>`, nil, ""},
		// 同理：样式表里的字面量也不算
		{"style 里的字面量不算", `<html><body><style>.captcha{}</style><p>正常正文</p></body></html>`, nil, ""},
		{"nil 安全", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var page *Page
			if c.html == "" {
				page = nil // nil 安全
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, _ = w.Write([]byte(c.html))
				}))
				defer server.Close()
				p, err := NewClient(Config{Timeout: time.Second}).Fetch(context.Background(), server.URL)
				if err != nil {
					t.Fatalf("Fetch = %v", err)
				}
				page = p
			}
			if got := RestrictedReason(page, c.extra...); got != c.want {
				t.Fatalf("RestrictedReason = %q, want %q", got, c.want)
			}
		})
	}
}

// Selection 存在的理由：业务把解析函数统一写成收 `*goquery.Selection`，线上传页面、
// 测试里传自己造的文档 —— 不用为"两种文档类型"再包一层接口。
func TestSelectionServesBothPageAndTestDocument(t *testing.T) {
	const body = `<html><body><ul><li data-id="1">A</li><li data-id="2">B</li></ul></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	// 一个只认 *goquery.Selection 的解析函数（业务里就是这个形状）
	parse := func(root *goquery.Selection) []string {
		ids := make([]string, 0)
		root.Find("li").Each(func(_ int, li *goquery.Selection) {
			if id, ok := li.Attr("data-id"); ok {
				ids = append(ids, id)
			}
		})
		return ids
	}

	client := NewClient(Config{Timeout: time.Second})
	page, err := client.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got := parse(page.Selection()); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("从页面取到的 = %#v", got)
	}
	// page.Document.Selection() 与 page.Selection() 是同一个根
	if page.Document.Selection() != page.Selection() {
		t.Fatal("两个入口应当给出同一个根 selection")
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewDocumentFromReader: %v", err)
	}
	if got := parse(doc.Selection); len(got) != 2 {
		t.Fatalf("从测试自造文档取到的 = %#v", got)
	}
}
