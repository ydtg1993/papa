package main

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
)

/* ---------- 参数解析 ---------- */

// 调试命令的参数可以写在位置参数之前或之后（`papa html --json <url>` 与 `papa html <url> --json` 都行）。
func TestParseDebugArgsAcceptsFlagsOnBothSides(t *testing.T) {
	df, pos, err := parseDebugArgs([]string{"--json", "https://example.com", "--links"})
	if err != nil {
		t.Fatalf("parseDebugArgs = %v", err)
	}
	if !df.jsonOut || !df.links {
		t.Fatalf("两个 flag 都该生效：%+v", df)
	}
	if len(pos) != 1 || pos[0] != "https://example.com" {
		t.Fatalf("位置参数 = %v", pos)
	}
}

// 带值的 flag 支持 `<flag> <值>` 与 `<flag>=<值>` 两种写法。
func TestParseDebugArgsValueForms(t *testing.T) {
	df, pos, err := parseDebugArgs([]string{
		"-c", "configs/a.yaml",
		"--timeout=20s",
		"--ua", "my-ua",
		"--header", "X-A=1",
		"--header=X-B=2",
		"-o", "out.html",
		"--output=out2.html",
		"--select", ".item",
		"--screenshot=shot.png",
		"--wait", "3s",
		"--act", "scroll:bottom",
		"--act=click:.more",
		"--full-page", "--proxy", "--show", "--devtools", "--text",
		"https://example.com",
	})
	if err != nil {
		t.Fatalf("parseDebugArgs = %v", err)
	}
	if df.config != "configs/a.yaml" {
		t.Fatalf("config = %q", df.config)
	}
	if df.timeout != "20s" {
		t.Fatalf("timeout = %q", df.timeout)
	}
	if df.ua != "my-ua" {
		t.Fatalf("ua = %q", df.ua)
	}
	if len(df.headers) != 2 || df.headers[0] != "X-A=1" || df.headers[1] != "X-B=2" {
		t.Fatalf("headers = %v", df.headers)
	}
	// -o 与 --output 写的是同一个字段，后者覆盖前者
	if df.output != "out2.html" {
		t.Fatalf("output = %q", df.output)
	}
	if df.selector != ".item" {
		t.Fatalf("selector = %q", df.selector)
	}
	if df.screenshot != "shot.png" || df.wait != "3s" || !df.fullPage {
		t.Fatalf("screenshot/wait/fullPage = %q/%q/%v", df.screenshot, df.wait, df.fullPage)
	}
	if len(df.acts) != 2 || df.acts[0] != "scroll:bottom" || df.acts[1] != "click:.more" {
		t.Fatalf("acts = %v（--act 可重复且保序）", df.acts)
	}
	if !df.proxy || !df.show || !df.devtools || !df.text {
		t.Fatalf("开关未生效：%+v", df)
	}
	if len(pos) != 1 {
		t.Fatalf("位置参数 = %v", pos)
	}
}

// 拼错的 flag 要在发请求之前就报出来，不能静默当位置参数吞掉。
func TestParseDebugArgsRejectsUnknownFlag(t *testing.T) {
	if _, _, err := parseDebugArgs([]string{"--nope", "https://example.com"}); err == nil {
		t.Fatal("未知 flag 应当报错")
	}
	// 单个 "-" 是位置参数（约定俗成的 stdin 占位），不算 flag
	df, pos, err := parseDebugArgs([]string{"-"})
	if err != nil {
		t.Fatalf("- 不该报错：%v", err)
	}
	if len(pos) != 1 || pos[0] != "-" || df.config != "" {
		t.Fatalf("pos=%v df=%+v", pos, df)
	}
}

// 带值 flag 后面什么都没有时报错，而不是把空串当值。
func TestParseDebugArgsMissingValues(t *testing.T) {
	for _, flag := range []string{"-c", "--config", "--timeout", "--ua", "--header", "-o", "--output", "--select", "--screenshot", "--wait", "--act"} {
		if _, _, err := parseDebugArgs([]string{flag}); err == nil {
			t.Fatalf("%s 缺少值应当报错", flag)
		}
	}
}

// 子命令的位置参数个数不对时给用法提示（而不是拿着残缺参数去抓取）。
func TestRunDebugCmdUsageErrors(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		args []string
	}{
		{"html", nil},
		{"html", []string{"a", "b"}},
		{"rod", nil},
		{"diff", []string{"a", "b"}},
		{"select", []string{"only-one"}},
		{"select", nil},
		{"unknown", []string{"https://example.com"}},
	} {
		if err := runDebugCmd(c.cmd, c.args); err == nil {
			t.Fatalf("runDebugCmd(%q, %v) 应当报错", c.cmd, c.args)
		}
	}
}

/* ---------- 配置与请求头 ---------- */

// 零配置也要能跑：配置读不到时返回零值 + false，命令继续用内置默认值。
func TestLoadOptionalConfigMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PAPA_CONFIG", "")

	cfg, ok := loadOptionalConfig("")
	if ok {
		t.Fatal("没有配置文件时 ok 应为 false")
	}
	if cfg == nil {
		t.Fatal("即便读不到也要返回非 nil，免得调用方解引用崩掉")
	}
	if cfg.HTML.Timeout != 0 || len(cfg.Browser.Headers) != 0 {
		t.Fatalf("读不到时应是零值配置：%+v", cfg)
	}
}

func TestLoadOptionalConfigFromPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\nhtml:\n  timeout: 7s\n  headers:\n    X-Cfg: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok := loadOptionalConfig(path)
	if !ok {
		t.Fatal("配置存在时 ok 应为 true")
	}
	if cfg.HTML.Timeout != 7*time.Second {
		t.Fatalf("html.timeout = %v, want 7s", cfg.HTML.Timeout)
	}
	if cfg.HTML.Headers["X-Cfg"] != "from-file" {
		t.Fatalf("配置头没读进来：%+v", cfg.HTML.Headers)
	}
}

// 没配代理地址就返回 nil —— 调用方据此完全不启用代理，而不是启用一个空地址的代理。
func TestNewProxyManagerWithoutAPIURL(t *testing.T) {
	if got := newProxyManager(&config.Config{}); got != nil {
		t.Fatalf("api_url 为空时不该建 Manager，实得 %+v", got)
	}
}

// 优先级：内置默认 < 配置 < --header；值两边的空白要吃掉，没带 "=" 的整条忽略。
func TestMergeHeadersPrecedence(t *testing.T) {
	got := mergeHeaders(
		map[string]string{"X-Cfg": "cfg", "User-Agent": "cfg-ua"},
		[]string{" X-Cli = cli ", "X-Cfg=cli-overrides-cfg", "malformed"},
	)

	if got["User-Agent"] != "cfg-ua" {
		t.Fatalf("配置应盖过内置默认：%q", got["User-Agent"])
	}
	if got["X-Cfg"] != "cli-overrides-cfg" {
		t.Fatalf("--header 应盖过配置：%q", got["X-Cfg"])
	}
	if got["X-Cli"] != "cli" {
		t.Fatalf("键值两边的空白应被裁掉：%q", got["X-Cli"])
	}
	if _, ok := got["malformed"]; ok {
		t.Fatal("没有 = 的 --header 应当被忽略")
	}
	// 没被覆盖的内置默认头还在
	if got["Accept-Language"] == "" || got["Accept"] == "" {
		t.Fatalf("内置默认头丢了：%+v", got)
	}
}

// 空串/非法时长都回退默认值：调试命令不该因为一个手滑的 --timeout 直接失败。
func TestParseTimeout(t *testing.T) {
	def := 15 * time.Second
	if got := parseTimeout("", def); got != def {
		t.Fatalf("空串应回退默认：%v", got)
	}
	if got := parseTimeout("2s", def); got != 2*time.Second {
		t.Fatalf("合法时长 = %v, want 2s", got)
	}
	if got := parseTimeout("2 seconds", def); got != def {
		t.Fatalf("非法时长应回退默认：%v", got)
	}
}

/* ---------- HTML 解析与输出 ---------- */

const probeHTML = `<!doctype html>
<html><head><title>  探针页  </title></head>
<body>
  <h1 class="t">标题一</h1>
  <h1 class="t">标题二</h1>
  <a href="/rel">相对</a>
  <a href="https://other.example.com/abs">绝对</a>
  <a href="/rel">重复</a>
  <a href="mailto:x@y.com">邮件</a>
  <a href="javascript:void(0)">脚本</a>
  <a>没有 href</a>
  <p>正文文本</p>
</body></html>`

func TestExtractTitleAndText(t *testing.T) {
	if got := extractTitle(probeHTML); got != "探针页" {
		t.Fatalf("extractTitle = %q（应裁掉空白）", got)
	}
	doc, err := parseDoc(probeHTML)
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(doc)
	for _, want := range []string{"标题一", "标题二", "正文文本"} {
		if !strings.Contains(text, want) {
			t.Fatalf("extractText 少了 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "<h1") {
		t.Fatalf("extractText 应当只取文本：\n%s", text)
	}
}

// 链接要绝对化、去重、只留 http(s)。
func TestExtractLinks(t *testing.T) {
	doc, err := parseDoc(probeHTML)
	if err != nil {
		t.Fatal(err)
	}
	got := extractLinks(doc, "https://example.com/dir/page.html")

	want := []string{"https://example.com/rel", "https://other.example.com/abs"}
	if len(got) != len(want) {
		t.Fatalf("extractLinks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extractLinks[%d] = %q, want %q（顺序为文档序）", i, got[i], want[i])
		}
	}
	for _, l := range got {
		if strings.HasPrefix(l, "mailto:") || strings.HasPrefix(l, "javascript:") {
			t.Fatalf("非 http(s) 链接不该保留：%q", l)
		}
	}
}

// 没有基址时相对的 href 解不出 http(s) scheme，会被一并滤掉 —— 只剩绝对链接。
// （命令里 --links 总是把最终 URL 当基址传进来，所以实际使用中不会走到这条窄路。）
func TestExtractLinksWithoutBase(t *testing.T) {
	doc, _ := parseDoc(probeHTML)
	got := extractLinks(doc, "")

	want := []string{"https://other.example.com/abs"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("extractLinks(doc, \"\") = %v, want %v", got, want)
	}
	// 基址非法（解析不出来）时同样只留绝对链接，且不 panic
	if links := extractLinks(doc, "://bad"); len(links) != 1 || links[0] != want[0] {
		t.Fatalf("非法基址下 = %v, want %v", links, want)
	}
}

func TestSelectMatches(t *testing.T) {
	doc, _ := parseDoc(probeHTML)
	got := selectMatches(doc, "h1.t")
	if len(got) != 2 || got[0] != "标题一" || got[1] != "标题二" {
		t.Fatalf("selectMatches = %v", got)
	}
	if got := selectMatches(doc, ".nope"); len(got) != 0 {
		t.Fatalf("无匹配应返回空：%v", got)
	}
}

// diff 的集合运算：common 计数、两边独有序（便于人读）。
func TestDiffLinks(t *testing.T) {
	a := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	b := map[string]struct{}{"y": {}, "z": {}, "w": {}}

	aOnly, bOnly, common := diffLinks(a, b)
	if common != 2 {
		t.Fatalf("common = %d, want 2", common)
	}
	if len(aOnly) != 1 || aOnly[0] != "x" {
		t.Fatalf("aOnly = %v, want [x]", aOnly)
	}
	if len(bOnly) != 1 || bOnly[0] != "w" {
		t.Fatalf("bOnly = %v, want [w]", bOnly)
	}

	// 空集合：返回空切片（不是 nil），JSON 里才会是 [] 而不是 null
	aOnly, bOnly, common = diffLinks(map[string]struct{}{}, map[string]struct{}{})
	if common != 0 || len(aOnly) != 0 || len(bOnly) != 0 {
		t.Fatalf("空集合：%v %v %d", aOnly, bOnly, common)
	}
	if aOnly == nil || bOnly == nil {
		t.Fatal("应返回空切片而不是 nil")
	}
}

func TestLinkSet(t *testing.T) {
	doc, _ := parseDoc(probeHTML)
	set := linkSet(doc, "https://example.com/")
	if len(set) != 2 {
		t.Fatalf("linkSet = %v", set)
	}
	if _, ok := set["https://example.com/rel"]; !ok {
		t.Fatalf("相对链接没被绝对化：%v", set)
	}
}

/* ---------- 输出形态 ---------- */

func TestEmitResultHTMLAndOutputFile(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "saved.html")
	meta := outcome{Engine: "html", InputURL: "https://example.com", HTMLLen: len(probeHTML)}

	got := captureStdout(t, func() {
		if err := emitResult(meta, probeHTML, "https://example.com", debugFlags{output: outFile}); err != nil {
			t.Errorf("emitResult = %v", err)
		}
	})

	// 只落盘时 stdout 应保持干净（提示走 stderr）
	if strings.TrimSpace(got) != "" {
		t.Fatalf("指定 -o 且形态为 html 时不该往 stdout 写内容，实得 %q", got)
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if string(b) != probeHTML {
		t.Fatal("落盘的 HTML 与输入不一致")
	}
}

func TestEmitResultLinksAndTextAndSelect(t *testing.T) {
	meta := outcome{Engine: "html"}

	got := captureStdout(t, func() {
		if err := emitResult(meta, probeHTML, "https://example.com/", debugFlags{links: true}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(got, "https://example.com/rel") || strings.Contains(got, "<a ") {
		t.Fatalf("--links 应逐行输出链接：\n%s", got)
	}

	got = captureStdout(t, func() {
		if err := emitResult(meta, probeHTML, "https://example.com/", debugFlags{text: true}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(got, "正文文本") {
		t.Fatalf("--text 应输出正文：\n%s", got)
	}

	got = captureStdout(t, func() {
		if err := emitResult(meta, probeHTML, "https://example.com/", debugFlags{selector: "h1.t"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(got, "0\t标题一") || !strings.Contains(got, "1\t标题二") {
		t.Fatalf("--select 应带序号输出：\n%s", got)
	}
}

func TestEmitJSONShapes(t *testing.T) {
	meta := outcome{Engine: "rod", InputURL: "https://example.com", Status: 200, Title: "t", HTMLLen: 10}

	// html 形态：直接回 meta
	got := captureStdout(t, func() {
		if err := emitJSON(meta, "html", nil, ""); err != nil {
			t.Fatal(err)
		}
	})
	var decoded outcome
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("html 形态应是一个 outcome 对象，实得 %s: %v", got, err)
	}
	if decoded.Engine != "rod" || decoded.Status != 200 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if strings.Contains(got, `"meta"`) {
		t.Fatalf("html 形态不该包一层 meta：%s", got)
	}

	// 内容形态：包成 meta + items
	got = captureStdout(t, func() {
		if err := emitJSON(meta, "links", []string{"a", "b"}, ""); err != nil {
			t.Fatal(err)
		}
	})
	var payload struct {
		Meta  outcome  `json:"meta"`
		Items []string `json:"items"`
		Text  string   `json:"text"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("解析: %v（原文 %s）", err, got)
	}
	if payload.Meta.Engine != "rod" || len(payload.Items) != 2 || payload.Text != "" {
		t.Fatalf("payload = %+v", payload)
	}

	// text 形态：text 有值、items 因 omitempty 缺席
	got = captureStdout(t, func() {
		if err := emitJSON(meta, "text", nil, "正文"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(got, `"text":"正文"`) || strings.Contains(got, `"items"`) {
		t.Fatalf("text 形态 = %s", got)
	}
}

/* ---------- runSelect 的本地文件分支 ---------- */

// target 不是 http(s) 时按本地文件解析 —— 这条路完全离线，正好覆盖 runSelect 的主干。
func TestRunSelectFromLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(path, []byte(probeHTML), 0o600); err != nil {
		t.Fatal(err)
	}

	got := captureStdout(t, func() {
		if err := runSelect("h1.t", path, debugFlags{}); err != nil {
			t.Errorf("runSelect = %v", err)
		}
	})
	if !strings.Contains(got, "0\t标题一") || !strings.Contains(got, "1\t标题二") {
		t.Fatalf("runSelect 输出 = %q", got)
	}

	got = captureStdout(t, func() {
		if err := runSelect("h1.t", path, debugFlags{jsonOut: true}); err != nil {
			t.Errorf("runSelect --json = %v", err)
		}
	})
	var payload struct {
		Selector string   `json:"selector"`
		Count    int      `json:"count"`
		Items    []string `json:"items"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("解析 --json 输出: %v（原文 %s）", err, got)
	}
	if payload.Selector != "h1.t" || payload.Count != 2 || len(payload.Items) != 2 {
		t.Fatalf("payload = %+v", payload)
	}

	// 文件不存在要报错，不能静默给空结果
	if err := runSelect("h1", filepath.Join(t.TempDir(), "nope.html"), debugFlags{}); err == nil {
		t.Fatal("读不到文件应当报错")
	}
}

/* ---------- 工具 ---------- */

// captureStdout 把 os.Stdout 换成管道，跑完 fn 后把写进去的内容取回来。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	_ = w.Close()
	os.Stdout = old
	got := <-done
	_ = r.Close()
	return got
}

// 相对路径要按基址解析（extractLinks 的绝对化依赖 url.ResolveReference）。
func TestExtractLinksResolveReference(t *testing.T) {
	base, _ := url.Parse("https://example.com/a/b/")
	doc, _ := parseDoc(`<a href="../up">u</a>`)
	got := extractLinks(doc, base.String())
	if len(got) != 1 || got[0] != "https://example.com/a/up" {
		t.Fatalf("相对路径解析 = %v", got)
	}
}
