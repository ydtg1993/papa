package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
)

// cliDefaultHeaders 零配置时的默认请求头，与引擎内置默认头保持一致。
var cliDefaultHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}

// debugFlags 调试子命令（html/rod/diff/select）共用的命令行参数。
type debugFlags struct {
	config     string
	proxy      bool
	timeout    string
	ua         string
	headers    []string
	jsonOut    bool
	output     string
	links      bool
	text       bool
	selector   string
	screenshot string
	fullPage   bool
	wait       string
	acts       []string
	show       bool
	devtools   bool
}

// parseDebugArgs 解析调试子命令参数，支持 flag 位于位置参数之前或之后。
func parseDebugArgs(args []string) (debugFlags, []string, error) {
	var df debugFlags
	var pos []string

	takeVal := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("flag %s requires a value", name)
		}
		*i++
		return args[*i], nil
	}

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.config = v
		case strings.HasPrefix(a, "--config="):
			df.config = strings.TrimPrefix(a, "--config=")
		case a == "--proxy":
			df.proxy = true
		case a == "--timeout":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.timeout = v
		case strings.HasPrefix(a, "--timeout="):
			df.timeout = strings.TrimPrefix(a, "--timeout=")
		case a == "--ua":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.ua = v
		case strings.HasPrefix(a, "--ua="):
			df.ua = strings.TrimPrefix(a, "--ua=")
		case a == "--header":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.headers = append(df.headers, v)
		case strings.HasPrefix(a, "--header="):
			df.headers = append(df.headers, strings.TrimPrefix(a, "--header="))
		case a == "--json":
			df.jsonOut = true
		case a == "-o" || a == "--output":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.output = v
		case strings.HasPrefix(a, "--output="):
			df.output = strings.TrimPrefix(a, "--output=")
		case a == "--links":
			df.links = true
		case a == "--text":
			df.text = true
		case a == "--select":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.selector = v
		case strings.HasPrefix(a, "--select="):
			df.selector = strings.TrimPrefix(a, "--select=")
		case a == "--screenshot":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.screenshot = v
		case strings.HasPrefix(a, "--screenshot="):
			df.screenshot = strings.TrimPrefix(a, "--screenshot=")
		case a == "--full-page":
			df.fullPage = true
		case a == "--wait":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.wait = v
		case strings.HasPrefix(a, "--wait="):
			df.wait = strings.TrimPrefix(a, "--wait=")
		case a == "--act":
			v, err := takeVal(&i, a)
			if err != nil {
				return df, nil, err
			}
			df.acts = append(df.acts, v)
		case strings.HasPrefix(a, "--act="):
			df.acts = append(df.acts, strings.TrimPrefix(a, "--act="))
		case a == "--show":
			df.show = true
		case a == "--devtools":
			df.devtools = true
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return df, nil, fmt.Errorf("unknown flag: %s", a)
			}
			pos = append(pos, a)
		}
		i++
	}
	return df, pos, nil
}

// runDebugCmd 分发调试子命令。
func runDebugCmd(cmd string, args []string) error {
	df, pos, err := parseDebugArgs(args)
	if err != nil {
		return err
	}
	switch cmd {
	case "html", "rod", "diff":
		if len(pos) != 1 {
			return fmt.Errorf("usage: papa %s <url> [flags]", cmd)
		}
		switch cmd {
		case "html":
			return runHTML(pos[0], df)
		case "rod":
			return runRod(pos[0], df)
		case "diff":
			return runDiff(pos[0], df)
		}
	case "select":
		if len(pos) != 2 {
			return fmt.Errorf("usage: papa select <css> <url|file> [flags]")
		}
		return runSelect(pos[0], pos[1], df)
	}
	return fmt.Errorf("unknown command: %s", cmd)
}

// loadOptionalConfig 加载配置；文件缺失时返回零值配置与 false，让命令零配置也能跑。
func loadOptionalConfig(path string) (*config.Config, bool) {
	cfgPath := path
	if cfgPath == "" {
		cfgPath = os.Getenv("PAPA_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "configs/config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return &config.Config{}, false
	}
	return cfg, true
}

// newProxyManager 仅在显式 --proxy 时构建代理管理器。
func newProxyManager(cfg *config.Config) *proxy.Manager {
	if cfg.Proxy.APIURL == "" {
		return nil
	}
	return proxy.NewManager(cfg.Proxy.APIURL, cfg.Proxy.RefreshInterval)
}

// mergeHeaders 按 内置默认 < 配置 < --header 覆盖 的顺序合并请求头。
func mergeHeaders(configHeaders map[string]string, overrides []string) map[string]string {
	out := make(map[string]string, len(cliDefaultHeaders)+len(configHeaders)+len(overrides))
	for k, v := range cliDefaultHeaders {
		out[k] = v
	}
	for k, v := range configHeaders {
		out[k] = v
	}
	for _, o := range overrides {
		if k, v, ok := strings.Cut(o, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// parseTimeout 解析时长字符串，失败或为空时返回默认值。
func parseTimeout(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

// outcome 结构化元数据，供 --json 输出。
type outcome struct {
	Engine      string `json:"engine"`
	InputURL    string `json:"input_url"`
	FinalURL    string `json:"final_url,omitempty"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Title       string `json:"title,omitempty"`
	HTMLLen     int    `json:"html_len"`
	ElapsedMs   int64  `json:"elapsed_ms,omitempty"`
}

// runHTML 静态 HTML 抓取（htmlfetch）。
func runHTML(rawURL string, df debugFlags) error {
	cfg, _ := loadOptionalConfig(df.config)
	headers := mergeHeaders(cfg.HTML.Headers, df.headers)

	ua := df.ua
	if ua == "" {
		ua = headers["User-Agent"]
	}
	delete(headers, "User-Agent")

	var pm *proxy.Manager
	if df.proxy {
		pm = newProxyManager(cfg)
	}

	client := htmlfetch.NewClient(htmlfetch.Config{
		Timeout:      parseTimeout(df.timeout, cfg.HTML.Timeout),
		MaxBodySize:  cfg.HTML.MaxBodySize,
		UserAgent:    ua,
		Headers:      headers,
		ProxyManager: pm,
	})

	start := time.Now()
	page, err := client.Fetch(context.Background(), rawURL)
	elapsed := time.Since(start)
	if err != nil {
		return err
	}

	out := outcome{
		Engine:      "html",
		InputURL:    rawURL,
		FinalURL:    page.URL.String(),
		Status:      page.StatusCode,
		ContentType: page.ContentType,
		Title:       extractTitle(page.HTML),
		HTMLLen:     len(page.HTML),
		ElapsedMs:   elapsed.Milliseconds(),
	}
	return emitResult(out, page.HTML, page.URL.String(), df)
}

// runRod 浏览器渲染后抓取（rod）。--act 顺序执行页面动作，--show 交还人工操作后再导出。
func runRod(rawURL string, df debugFlags) error {
	cfg, loaded := loadOptionalConfig(df.config)
	headers := mergeHeaders(cfg.Browser.Headers, df.headers)

	// 解析动作序列：语法错误在开浏览器之前就报出来
	acts, err := parseActs(df.acts)
	if err != nil {
		return err
	}

	headless := cfg.Browser.Headless
	if !loaded {
		headless = true
	}
	if df.show || df.devtools {
		headless = false
	}

	flags := map[string]string{}
	if df.devtools {
		flags["auto-open-devtools-for-tabs"] = ""
	}

	var pm *proxy.Manager
	if df.proxy {
		pm = newProxyManager(cfg)
	}

	pool, err := browser.NewPool(browser.PoolConfig{
		Size:           1,
		DirectSize:     0,
		MaxIdleTime:    0,
		Headless:       headless,
		NoSandbox:      cfg.Browser.NoSandbox,
		Leakless:       cfg.Browser.Leakless,
		BrowserPath:    cfg.Browser.BrowserPath,
		Flags:          flags,
		DefaultHeaders: headers,
		ProxyManager:   pm,
	})
	if err != nil {
		return fmt.Errorf("create browser: %w", err)
	}
	defer pool.Close()

	ctx := context.Background()
	b, err := pool.Get(ctx)
	if err != nil {
		return err
	}
	defer pool.Put(b)

	actTimeout := parseTimeout(df.timeout, actDefaultTimeout)
	start := time.Now()
	res, err := b.FetchOnce(ctx, rawURL, browser.PageOptions{
		Timeout:    parseTimeout(df.timeout, 0),
		Wait:       parseTimeout(df.wait, 0),
		Screenshot: df.screenshot != "",
		FullPage:   df.fullPage,
		Interact: func(_ context.Context, page *rod.Page) error {
			if err := runActs(page, acts, actTimeout, os.Stderr); err != nil {
				return err
			}
			if df.show || df.devtools {
				return waitEnter()
			}
			return nil
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		return err
	}

	if df.screenshot != "" {
		if err := os.WriteFile(df.screenshot, res.Screenshot, 0o644); err != nil {
			return fmt.Errorf("write screenshot: %w", err)
		}
		fmt.Fprintf(os.Stderr, "screenshot saved to %s\n", df.screenshot)
	}

	out := outcome{
		Engine:      "rod",
		InputURL:    rawURL,
		FinalURL:    res.FinalURL,
		Status:      res.Status,
		ContentType: res.ContentType,
		Title:       res.Title,
		HTMLLen:     len(res.HTML),
		ElapsedMs:   elapsed.Milliseconds(),
	}
	return emitResult(out, res.HTML, res.FinalURL, df)
}

// runDiff 同一 URL 分别用 htmlfetch 与 rod 抓取并对比。
func runDiff(rawURL string, df debugFlags) error {
	cfg, loaded := loadOptionalConfig(df.config)

	// html 侧
	htmlHeaders := mergeHeaders(cfg.HTML.Headers, df.headers)
	ua := df.ua
	if ua == "" {
		ua = htmlHeaders["User-Agent"]
	}
	delete(htmlHeaders, "User-Agent")

	var pm *proxy.Manager
	if df.proxy {
		pm = newProxyManager(cfg)
	}

	htmlClient := htmlfetch.NewClient(htmlfetch.Config{
		Timeout:      parseTimeout(df.timeout, cfg.HTML.Timeout),
		MaxBodySize:  cfg.HTML.MaxBodySize,
		UserAgent:    ua,
		Headers:      htmlHeaders,
		ProxyManager: pm,
	})
	htmlPage, err := htmlClient.Fetch(context.Background(), rawURL)
	if err != nil {
		return fmt.Errorf("html fetch: %w", err)
	}

	// rod 侧
	headless := cfg.Browser.Headless
	if !loaded {
		headless = true
	}
	pool, err := browser.NewPool(browser.PoolConfig{
		Size:           1,
		DirectSize:     0,
		Headless:       headless,
		NoSandbox:      cfg.Browser.NoSandbox,
		Leakless:       cfg.Browser.Leakless,
		BrowserPath:    cfg.Browser.BrowserPath,
		Flags:          map[string]string{},
		DefaultHeaders: mergeHeaders(cfg.Browser.Headers, df.headers),
		ProxyManager:   pm,
	})
	if err != nil {
		return fmt.Errorf("create browser: %w", err)
	}
	defer pool.Close()

	ctx := context.Background()
	b, err := pool.Get(ctx)
	if err != nil {
		return err
	}
	defer pool.Put(b)

	rodRes, err := b.FetchOnce(ctx, rawURL, browser.PageOptions{
		Timeout: parseTimeout(df.timeout, 0),
	})
	if err != nil {
		return fmt.Errorf("rod fetch: %w", err)
	}

	htmlDoc, _ := parseDoc(htmlPage.HTML)
	rodDoc, _ := parseDoc(rodRes.HTML)
	htmlOnly, rodOnly, common := diffLinks(
		linkSet(htmlDoc, htmlPage.URL.String()),
		linkSet(rodDoc, rodRes.FinalURL),
	)

	if df.jsonOut {
		report := map[string]any{
			"input_url": rawURL,
			"html": map[string]any{
				"status":    htmlPage.StatusCode,
				"final_url": htmlPage.URL.String(),
				"html_len":  len(htmlPage.HTML),
			},
			"rod": map[string]any{
				"status":    rodRes.Status,
				"final_url": rodRes.FinalURL,
				"html_len":  len(rodRes.HTML),
			},
			"html_equal": htmlPage.HTML == rodRes.HTML,
			"links": map[string]any{
				"html_only": htmlOnly,
				"rod_only":  rodOnly,
				"common":    common,
			},
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}

	fmt.Printf("input: %s\n", rawURL)
	fmt.Printf("html: status=%d final=%s len=%d\n", htmlPage.StatusCode, htmlPage.URL.String(), len(htmlPage.HTML))
	fmt.Printf("rod:  status=%d final=%s len=%d\n", rodRes.Status, rodRes.FinalURL, len(rodRes.HTML))
	fmt.Printf("html_equal: %t\n", htmlPage.HTML == rodRes.HTML)
	fmt.Printf("links: common=%d html_only=%d rod_only=%d\n", common, len(htmlOnly), len(rodOnly))
	for _, l := range htmlOnly {
		fmt.Printf("  html_only: %s\n", l)
	}
	for _, l := range rodOnly {
		fmt.Printf("  rod_only: %s\n", l)
	}
	return nil
}

// runSelect 选择器测试：target 为 URL 时抓取后查询，否则按本地 html 文件解析。
func runSelect(css string, target string, df debugFlags) error {
	var htmlStr string

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		cfg, _ := loadOptionalConfig(df.config)
		headers := mergeHeaders(cfg.HTML.Headers, df.headers)
		ua := df.ua
		if ua == "" {
			ua = headers["User-Agent"]
		}
		delete(headers, "User-Agent")

		var pm *proxy.Manager
		if df.proxy {
			pm = newProxyManager(cfg)
		}
		client := htmlfetch.NewClient(htmlfetch.Config{
			Timeout:      parseTimeout(df.timeout, cfg.HTML.Timeout),
			MaxBodySize:  cfg.HTML.MaxBodySize,
			UserAgent:    ua,
			Headers:      headers,
			ProxyManager: pm,
		})
		page, err := client.Fetch(context.Background(), target)
		if err != nil {
			return err
		}
		htmlStr = page.HTML
	} else {
		b, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
		htmlStr = string(b)
	}

	doc, err := parseDoc(htmlStr)
	if err != nil {
		return err
	}
	matches := selectMatches(doc, css)
	if df.jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"selector": css,
			"count":    len(matches),
			"items":    matches,
		})
	}
	for i, m := range matches {
		fmt.Printf("%d\t%s\n", i, m)
	}
	return nil
}

// emitResult 按输出形态（html/links/text/select）与 --json/-o 输出抓取结果。
func emitResult(out outcome, htmlStr, baseURL string, df debugFlags) error {
	doc, err := parseDoc(htmlStr)
	if err != nil {
		return err
	}

	mode := "html"
	var items []string
	var text string
	switch {
	case df.links:
		mode = "links"
		items = extractLinks(doc, baseURL)
	case df.text:
		mode = "text"
		text = extractText(doc)
	case df.selector != "":
		mode = "select"
		items = selectMatches(doc, df.selector)
	}

	// HTML 落盘（可选）
	if df.output != "" {
		if err := os.WriteFile(df.output, []byte(htmlStr), 0o644); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}

	if df.jsonOut {
		return emitJSON(out, mode, items, text)
	}

	// 非 JSON：只落盘时静默（提示到 stderr）
	if df.output != "" && mode == "html" {
		fmt.Fprintf(os.Stderr, "html saved to %s\n", df.output)
		return nil
	}
	switch mode {
	case "html":
		fmt.Println(htmlStr)
	case "links":
		for _, it := range items {
			fmt.Println(it)
		}
	case "text":
		fmt.Println(text)
	case "select":
		for i, it := range items {
			fmt.Printf("%d\t%s\n", i, it)
		}
	}
	return nil
}

// emitJSON 输出 --json；带 items/text 的内容形态包装成 meta+items。
func emitJSON(out outcome, mode string, items []string, text string) error {
	if mode == "html" {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	payload := struct {
		Meta  outcome  `json:"meta"`
		Items []string `json:"items,omitempty"`
		Text  string   `json:"text,omitempty"`
	}{Meta: out, Items: items, Text: text}
	return json.NewEncoder(os.Stdout).Encode(payload)
}

// parseDoc 解析 HTML 字符串为 goquery 文档。
func parseDoc(htmlStr string) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
}

// extractTitle 提取 <title> 文本。
func extractTitle(htmlStr string) string {
	doc, err := parseDoc(htmlStr)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Find("title").First().Text())
}

// extractText 提取 <body> 可读文本。
func extractText(doc *goquery.Document) string {
	return strings.TrimSpace(doc.Find("body").Text())
}

// extractLinks 提取所有 <a href> 并绝对化（按 baseURL），去重，仅保留 http(s)。
func extractLinks(doc *goquery.Document, baseURL string) []string {
	var base *url.URL
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil {
			base = u
		}
	}
	seen := make(map[string]struct{})
	var out []string
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		if href == "" {
			return
		}
		abs := href
		if base != nil {
			if ref, err := url.Parse(href); err == nil {
				abs = base.ResolveReference(ref).String()
			}
		}
		if u, err := url.Parse(abs); err == nil && u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	})
	return out
}

// selectMatches 返回选择器所有匹配节点的文本。
func selectMatches(doc *goquery.Document, selector string) []string {
	var out []string
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		out = append(out, strings.TrimSpace(s.Text()))
	})
	return out
}

// linkSet 将链接列表转为集合，供 diff 使用。
func linkSet(doc *goquery.Document, base string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, l := range extractLinks(doc, base) {
		out[l] = struct{}{}
	}
	return out
}

// diffLinks 计算两个链接集合的差异。
func diffLinks(a, b map[string]struct{}) (aOnly, bOnly []string, common int) {
	aOnly = make([]string, 0)
	bOnly = make([]string, 0)
	for k := range a {
		if _, ok := b[k]; ok {
			common++
		} else {
			aOnly = append(aOnly, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			bOnly = append(bOnly, k)
		}
	}
	sort.Strings(aOnly)
	sort.Strings(bOnly)
	return
}
