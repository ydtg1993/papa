package htmlfetch

import "testing"

func TestRestrictedReasonIgnoresNonvisibleHTML(t *testing.T) {
	for _, fragment := range []string{
		`<template id="challenge"><p>请填写验证码</p></template>`,
		`<template><title>CAPTCHA</title></template>`,
		`<div hidden>请填写验证码</div>`,
		`<section hidden="false"><p>CAPTCHA</p></section>`,
		`<div hidden="until-found"><p>验证码</p></div>`,
		`<dialog><p>验证码</p></dialog>`,
		`<details><summary>登录帮助</summary><p>验证码</p></details>`,
		`<details><summary hidden>帮助</summary><summary>验证码</summary></details>`,
		`<details>验证码<summary>帮助</summary></details>`,
	} {
		t.Run(fragment, func(t *testing.T) {
			page, err := NewRenderedPage("https://example.test/catalog", `<html><body><h1>正常列表</h1>`+fragment+`</body></html>`)
			if err != nil {
				t.Fatal(err)
			}
			before, err := page.Selection().Html()
			if err != nil {
				t.Fatal(err)
			}
			if marker := RestrictedReason(page); marker != "" {
				t.Errorf("nonvisible HTML triggered restricted-page marker %q", marker)
			}
			after, err := page.Selection().Html()
			if err != nil || before != after {
				t.Errorf("restricted-page detection modified the original document: %v", err)
			}
		})
	}
	page, err := NewRenderedPage("https://example.test/catalog", `<html><body hidden><p>验证码</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if marker := RestrictedReason(page); marker != "" {
		t.Errorf("hidden body triggered restricted-page marker %q", marker)
	}
}

func TestRestrictedReasonPreservesVisibleHTML(t *testing.T) {
	for _, fragment := range []string{
		`<template>正常内容</template><p>验证码</p>`,
		`<div hidden>正常内容</div><p>验证码</p>`,
		`<div aria-hidden="true">验证码</div>`, // ARIA 不控制视觉显示。
		`<div inert>验证码</div>`,              // inert 禁用交互，不隐藏元素。
		`<dialog open>验证码</dialog>`,
		`<details><summary>验证码</summary><p>正常内容</p></details>`,
		`<details open><summary>帮助</summary><p>验证码</p></details>`,
	} {
		t.Run(fragment, func(t *testing.T) {
			page, err := NewRenderedPage("https://example.test/challenge", `<html><body>`+fragment+`</body></html>`)
			if err != nil {
				t.Fatal(err)
			}
			if marker := RestrictedReason(page); marker != "验证码" {
				t.Errorf("visible restricted-page marker = %q, want 验证码", marker)
			}
		})
	}
}

func TestRestrictedReasonScopesHiddenRulesToHTMLNamespace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fragment string
		want     string
	}{
		{"html_hidden", `<span hidden>验证码</span>`, ""},
		{"svg_hidden_text", `<svg><text hidden>验证码</text></svg>`, "验证码"},
		{"svg_hidden_root", `<svg hidden><text>验证码</text></svg>`, "验证码"},
		{"html_hidden_ancestor_of_svg", `<div hidden><svg><text>验证码</text></svg></div>`, ""},
		{"foreign_object_visible_html", `<svg><foreignObject><p>验证码</p></foreignObject></svg>`, "验证码"},
		{"foreign_object_hidden_html", `<svg><foreignObject><p hidden>验证码</p></foreignObject></svg>`, ""},
		{"foreign_object_template", `<svg><foreignObject><template>验证码</template></foreignObject></svg>`, ""},
		{"foreign_object_closed_dialog", `<svg><foreignObject><dialog>验证码</dialog></foreignObject></svg>`, ""},
		{"foreign_object_open_dialog", `<svg><foreignObject><dialog open>验证码</dialog></foreignObject></svg>`, "验证码"},
		{"foreign_object_closed_details", `<svg><foreignObject><details><summary>帮助</summary><p>验证码</p></details></foreignObject></svg>`, ""},
		{"foreign_object_details_summary", `<svg><foreignObject><details><summary>验证码</summary><p>帮助</p></details></foreignObject></svg>`, "验证码"},
		{"foreign_object_nested_svg_hidden", `<svg><foreignObject><div><svg hidden><text hidden>验证码</text></svg></div></foreignObject></svg>`, "验证码"},
		{"nested_svg_foreign_object_hidden_html", `<svg><foreignObject><svg><foreignObject><p hidden>验证码</p></foreignObject></svg></foreignObject></svg>`, ""},
		// 同名 SVG 元素不具有 HTML 的 template/dialog/details 固有隐藏语义。
		{"svg_template", `<svg><template><text>验证码</text></template></svg>`, "验证码"},
		{"svg_dialog", `<svg><dialog><text>验证码</text></dialog></svg>`, "验证码"},
		{"svg_details", `<svg><details><text>验证码</text></details></svg>`, "验证码"},
		// script/style 继续沿用原有的跨 namespace 排除语义。
		{"svg_script", `<svg><script>验证码</script></svg>`, ""},
		{"svg_style", `<svg><style>验证码</style></svg>`, ""},
		// SVG title 的既有文本提取边界不在本次修复范围内。
		{"svg_title_retains_legacy_text", `<svg><title>验证码</title><text>正文</text></svg>`, "验证码"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := NewRenderedPage("https://example.test/challenge", `<html><body>`+tc.fragment+`</body></html>`)
			if err != nil {
				t.Fatal(err)
			}
			before, err := page.Selection().Html()
			if err != nil {
				t.Fatal(err)
			}
			if got := RestrictedReason(page); got != tc.want {
				t.Errorf("restricted-page marker = %q, want %q", got, tc.want)
			}
			after, err := page.Selection().Html()
			if err != nil || before != after {
				t.Errorf("restricted-page detection modified the original document: %v", err)
			}
		})
	}
}

func TestRestrictedReasonSelectsFirstHTMLSummary(t *testing.T) {
	page, err := NewRenderedPage("https://example.test/challenge", `<html><body><details><summary>普通标签</summary><summary>验证码</summary><p>正文</p></details></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	// Selection 允许业务操作 DOM；构造一个先于 HTML summary 的 SVG 同名节点。
	// 不能通过 HTML 字符串表达这个直接子节点，否则解析器会把它重新归入 HTML namespace。
	foreignSummary := page.Selection().Find("summary").First().Get(0)
	foreignSummary.Namespace = "svg"
	before, err := page.Selection().Html()
	if err != nil {
		t.Fatal(err)
	}
	if got := RestrictedReason(page); got != "验证码" {
		t.Errorf("restricted-page marker from first HTML summary = %q, want 验证码", got)
	}
	after, err := page.Selection().Html()
	if err != nil || before != after {
		t.Errorf("restricted-page detection modified the original document: %v", err)
	}
	if foreignSummary.Namespace != "svg" {
		t.Errorf("restricted-page detection changed original summary namespace to %q", foreignSummary.Namespace)
	}
}

func TestVisibleTextPreservesTextConcatenation(t *testing.T) {
	page, err := NewRenderedPage("https://example.test/content", `<html><body>pre<span>fix</span><div hidden>hidden</div><svg hidden><text>中</text></svg><template>hidden</template><details><summary>文</summary>hidden</details><script>hidden</script><style>hidden</style><dialog>hidden</dialog> suffix</body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleText(page.Document); got != "prefix中文 suffix" {
		t.Errorf("visible text = %q, want %q", got, "prefix中文 suffix")
	}
}
