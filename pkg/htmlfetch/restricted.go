package htmlfetch

import (
	"strings"

	"golang.org/x/net/html"
)

// 本文件是**受限页（被反爬拦下）的默认判据**：每个爬虫项目都要写一遍"这页是不是 captcha/访问受限"，
// 而写法大同小异 —— 框架给一份默认的，业务按站点追加自己的文案即可。
//
// 配合 `papa.RestrictedPageError` 用：
//
//	page, err := engine.FetchHTML(ctx, url)
//	if err != nil { return err }
//	if reason := htmlfetch.RestrictedReason(page, site.RestrictedKeywords...); reason != "" {
//	    return papa.RestrictedPageError("catalog", page.URL.String(), reason)
//	}

// defaultRestrictedMarkers 默认词表。**宁少勿多**：命中之后业务一般返回**不可重试**错误
// （任务直接判死），一个假阳性就是一条任务白死，所以只放几乎不可能出现在正常页面正文里的短语。
// 站点特有的文案（"安全验证""请稍后再试"之类）由业务通过 extra 追加。
var defaultRestrictedMarkers = []string{
	"captcha",
	"verify you are human",
	"access denied",
	"访问受限",
	"验证码",
	"checking your browser", // Cloudflare 拦截页（"Checking your browser before accessing…"）
	"just a moment",         // 同上，标题就是 "Just a moment..."
	"unusual traffic",       // Google："We're sorry... detected unusual traffic from your computer network"
}

// RestrictedReason 判断这一页看起来是不是"被反爬拦下了"：命中时返回**命中的那句话**
// （写进错误/trace 里便于排查），否则返回空串。extra 是业务追加的本站文案。
//
// 判据扫的是**可见正文 + 标题**里的短语，大小写不敏感。刻意**不扫整段 HTML** ——
// 正常页面里也有 `<script src="...recaptcha...">` 这类字面量，扫原始 HTML 会把好页面判成受限页。
//
// 它是**判据**不是**决定**：要不要因此判任务失败由业务定 —— 想先观察一阵，可以只
// `task.Trace.Warn("疑似受限页", nil, map[string]string{"marker": reason})` 而不返回错误。
func RestrictedReason(page *Page, extra ...string) string {
	if page == nil || page.Document == nil {
		return ""
	}
	// 标题单列：Cloudflare 那类拦截页的正文字数很少，关键短语常常只在标题里
	title, _ := page.Document.Text("head > title")
	haystack := strings.ToLower(title + "\n" + visibleText(page.Document))
	if strings.TrimSpace(haystack) == "" {
		return ""
	}
	for _, m := range defaultRestrictedMarkers {
		if strings.Contains(haystack, strings.ToLower(m)) {
			return m
		}
	}
	for _, m := range extra {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.Contains(haystack, strings.ToLower(m)) {
			return m
		}
	}
	return ""
}

// visibleText 按 HTML 固有的显示语义取正文，不推测 CSS 或脚本控制的可见性。
//
// HTML 模板、hidden 元素、未打开的 dialog 和折叠 details 的正文不属于页面当前显示的内容。
// 只读遍历原文档；aria-hidden / inert 不控制视觉显示，不能据此排除。
func visibleText(doc *Document) string {
	if doc == nil || doc.selection == nil {
		return ""
	}
	var text strings.Builder
	for _, body := range doc.selection.Find("body").Nodes {
		if body.Namespace != "" {
			continue
		}
		hiddenAncestor := false
		for parent := body.Parent; parent != nil; parent = parent.Parent {
			if parent.Namespace == "" && hasHTMLAttribute(parent, "hidden") {
				hiddenAncestor = true
				break
			}
		}
		if !hiddenAncestor {
			appendVisibleText(&text, body)
		}
	}
	return text.String()
}

func appendVisibleText(text *strings.Builder, node *html.Node) {
	if node.Type == html.TextNode {
		text.WriteString(node.Data)
		return
	}
	if node.Type == html.ElementNode {
		// script/style 继续跨 namespace 排除；其余规则仅属于 HTML 固有的显示语义。
		if node.Data == "script" || node.Data == "style" {
			return
		}
		// x/net/html 用空 namespace 表示 HTML。直接遍历保留 SVG/foreignObject 的
		// namespace；goquery v1.10.2 的 Clone 不复制它，克隆后再判断会误删 SVG。
		if node.Namespace == "" {
			if node.Data == "template" || hasHTMLAttribute(node, "hidden") ||
				(node.Data == "dialog" && !hasHTMLAttribute(node, "open")) {
				return
			}
			if node.Data == "details" && !hasHTMLAttribute(node, "open") {
				// 折叠时只有第一个 HTML 直接子 summary 显示，也跳过裸文本节点。
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					if child.Type == html.ElementNode && child.Namespace == "" && child.Data == "summary" {
						appendVisibleText(text, child)
						break
					}
				}
				return
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendVisibleText(text, child)
	}
}

func hasHTMLAttribute(node *html.Node, name string) bool {
	for _, attr := range node.Attr {
		if attr.Namespace == "" && attr.Key == name {
			return true
		}
	}
	return false
}
