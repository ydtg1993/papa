package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ---------- 初始化选项 ---------- */

func TestOptionsSetFields(t *testing.T) {
	var a App
	if err := WithConfigPath("configs/x.yaml")(&a); err != nil {
		t.Fatalf("WithConfigPath = %v", err)
	}
	if a.configPath != "configs/x.yaml" {
		t.Fatalf("configPath = %q", a.configPath)
	}

	if err := WithModels(&struct{}{}, &struct{}{})(&a); err != nil {
		t.Fatalf("WithModels = %v", err)
	}
	if len(a.extraModels) != 2 {
		t.Fatalf("WithModels 应追加两个模型，实得 %d", len(a.extraModels))
	}

	// UseModels 与 WithModels 等价，区别只是能在 New 之后调用（脚手架就是那样登记的）
	a.UseModels(&struct{}{})
	if len(a.extraModels) != 3 {
		t.Fatalf("UseModels 应追加到同一份清单，实得 %d", len(a.extraModels))
	}
}

// key 会进 DOM id 与 URL，所以限制成字母/数字/下划线/连字符。
func TestValidPageKey(t *testing.T) {
	ok := []string{"review", "review-2", "review_v2", "A1", "0"}
	for _, k := range ok {
		if !validPageKey(k) {
			t.Errorf("validPageKey(%q) = false, want true", k)
		}
	}
	bad := []string{"", "re view", "re/view", "re.view", "re?x", "中文", "a\nb", "re#"}
	for _, k := range bad {
		if validPageKey(k) {
			t.Errorf("validPageKey(%q) = true, want false", k)
		}
	}
}

/* ---------- 注入脚本与样式 ---------- */

func TestUseScriptAndCSSIgnoreBlank(t *testing.T) {
	a := &App{}
	a.UseScript("")
	a.UseScript("   \n\t ")
	a.UseCSS("")
	a.UseCSS("\n")

	if len(a.extraJS) != 0 || len(a.extraCSS) != 0 {
		t.Fatalf("空白内容不该被记下来：js=%v css=%v", a.extraJS, a.extraCSS)
	}

	a.UseScript("window.x = 1;")
	a.UseCSS(".x { color: red; }")
	if len(a.extraJS) != 1 || len(a.extraCSS) != 1 {
		t.Fatalf("正常内容应被追加：js=%v css=%v", a.extraJS, a.extraCSS)
	}
}

// 读不到文件直接 panic：这是部署问题（路径写错 / 文件没打进去），
// 静默跳过会让"样式怎么没生效"变成一个查不动的问题。
func TestUseScriptFileAndCSSFile(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "custom.js")
	css := filepath.Join(dir, "custom.css")
	if err := os.WriteFile(js, []byte("window.injected = 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(css, []byte(".injected{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	a.UseScriptFile(js)
	a.UseCSSFile(css)

	if len(a.extraJS) != 1 || !strings.Contains(a.extraJS[0], "injected") {
		t.Fatalf("JS 没读进来：%v", a.extraJS)
	}
	if len(a.extraCSS) != 1 || !strings.Contains(a.extraCSS[0], ".injected") {
		t.Fatalf("CSS 没读进来：%v", a.extraCSS)
	}

	for _, c := range []struct {
		name string
		use  func()
	}{
		{"JS 文件不存在", func() { (&App{}).UseScriptFile(filepath.Join(dir, "nope.js")) }},
		{"CSS 文件不存在", func() { (&App{}).UseCSSFile(filepath.Join(dir, "nope.css")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("读不到文件应当 panic（部署问题不该静默）")
				}
			}()
			c.use()
		})
	}
}

// UseRouter 的 nil 回调与路由组累加已在 router_test.go 里覆盖。

func TestUseRouterAndTablesAccumulate(t *testing.T) {
	a := &App{}
	a.UseRouter(func(*Router) {})
	a.UseRouter(func(*Router) {})
	if len(a.routers) != 2 {
		t.Fatalf("routers = %d, want 2", len(a.routers))
	}
}
