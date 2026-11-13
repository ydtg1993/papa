package core

import (
	"fmt"
	"net/url"
	"strings"
)

// PageIdentity supplies optional host-independent resource identifiers.
// Durable task identity and site/stage scoping belong to NormalizeTaskIdentity.

// PageIdentity 把页面地址规范成与站点域名无关的身份：`path[?query]`，去掉 fragment。
//
//   - 只接受绝对的 http/https 地址，且不带 userinfo；
//   - path 为空时按 `/` 处理（`https://a.com` 与 `https://a.com/` 是同一页）；
//   - path 用 **EscapedPath**（`/a%2Fb` 与 `/a/b` 是不同资源，不能被解成同一个身份）；
//   - query 原样保留（顺序有意义，V1 不做排序）。
//
// host 被忽略是有意的：租户隔离靠 NormalizeTaskIdentity 的 Site/Stage。这是「身份」不是「范围」。
func PageIdentity(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", fmt.Errorf("page identity: empty URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("page identity: parse URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("page identity: unsupported scheme %q in %q", parsed.Scheme, raw)
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("page identity: URL has no host: %q", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("page identity: URL carries userinfo: %q", raw)
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("page identity: URL path %q does not start with /", path)
	}
	if parsed.RawQuery != "" {
		return path + "?" + parsed.RawQuery, nil
	}
	return path, nil
}
