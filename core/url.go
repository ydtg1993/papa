package core

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 本文件是**页面链接解析**：把页面上那些相对地址变成绝对地址，并把"是不是本站"这件事判对。
//
// 为什么进框架：这两件事每个多页爬虫都要写一遍，而且都容易写错 ——
//   - 解析漏掉 scheme/fragment 处理 → `javascript:` 的"链接"进了任务队列、`/a#x` 与 `/a#y` 各抓一次；
//   - 同站判定用 `strings.HasSuffix(host, "example.com")` → 把 `evil-example.com` 也当成自己人。
//
// 策略仍在业务手里：这里只回答"解析出来是什么"和"是不是同一个 host"，不替谁决定要不要跟。

// ResolveURL 把页面上的相对地址按 base 解析成绝对地址。
//
// 三件事是刻意做的：
//   - **只收 http/https**：`javascript:` / `mailto:` / `tel:` 这类"链接"不该进任务队列；
//   - **去掉 fragment**：`/a#x` 与 `/a#y` 是同一页，留着会让同一页被抓两次；
//   - 解析不出 host 就报错：残缺地址宁可当场报错，也别在后面拼出半截 URL。
func ResolveURL(base, reference string) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", base, err)
	}
	ref, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", fmt.Errorf("parse reference URL %q: %w", reference, err)
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q in %q", resolved.Scheme, reference)
	}
	if resolved.Host == "" {
		return "", fmt.Errorf("resolved URL has no host: %q", resolved.String())
	}
	resolved.Fragment = ""
	return resolved.String(), nil
}

// SameHost 判断两个地址（URL 或裸 host 都收）是不是同一个 host：
// 忽略大小写、端口与末尾的点，并把 `www.` 前缀视作同一个 host。
//
//	SameHost("https://example.com/a", "www.example.com") == true
//
// 为什么 www 算同一个：爬虫里 apex 与 www 几乎总是同一个站（互相跳转、同一套内容），
// 分开算会让"本站链接"的判定莫名其妙地失败。
func SameHost(a, b string) bool {
	hostA := hostOf(a)
	return hostA != "" && hostA == hostOf(b)
}

// IsSubdomainOf 判断 child 是不是 parent 的子域，按**点边界**比对 ——
// `evil-example.com` **不是** `example.com` 的子域（`strings.HasSuffix` 最容易在这儿翻车）。
//
// 两者都按 SameHost 那样归一化，所以 `IsSubdomainOf("example.com", "www.example.com")` 是
// **false**（那是同一个 host，用 SameHost 判）。CDN 之类的资源域名判定用它：
//
//	if !core.SameHost(base, u) && !core.IsSubdomainOf(u, base) { return fmt.Errorf("站外资源 %s", u) }
func IsSubdomainOf(child, parent string) bool {
	childHost, parentHost := hostOf(child), hostOf(parent)
	if childHost == "" || parentHost == "" {
		return false
	}
	return strings.HasSuffix(childHost, "."+parentHost)
}

// hostOf 从 URL 或裸 host 里取出归一化后的 host（小写、去端口、去末尾点、去 www. 前缀）。
// 不是 URL 也认（裸 host 喂进来是常事），拿不准时一律按"整个字符串就是 host"处理。
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	host := raw
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		host = parsed.Host
	} else if parsed, err := url.Parse("//" + raw); err == nil && parsed.Host != "" {
		// 裸 host（可能带端口）走这条：`example.com:8080` 直接 Parse 会把 example.com 当 scheme
		host = parsed.Host
	}
	host = strings.ToLower(host)
	if withoutPort, _, err := net.SplitHostPort(host); err == nil {
		host = withoutPort
	}
	host = strings.TrimSuffix(host, ".")
	return strings.TrimPrefix(host, "www.")
}
