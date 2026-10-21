package core

import (
	"fmt"
	"net/url"
	"strings"
)

// ResolveSiteURL 把 raw 解析成「当前站点根地址」下的绝对地址：
//
//   - scheme / host 取自 baseURL；
//   - path / query 取自 raw（raw 可以是相对地址；空串得到站点根 `/`）；
//   - fragment 去掉；拒绝 userinfo、非 http(s)、无 host 的 base。
//
// 注意：raw 如果是别的域名上的绝对地址，host 会被**改写**成 baseURL 的 host。
// 这是给「任务里存着旧域名」的场景用的（站点换域名后按当前 BaseURL 重新发请求），
// 不是通用的链接跟随函数 —— 跟链接请用 ResolveURL + SameHost。
func ResolveSiteURL(baseURL, raw string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil {
		return "", fmt.Errorf("invalid site base URL %q", baseURL)
	}
	absolute, err := ResolveURL(base.Scheme+"://"+base.Host+"/", raw)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(absolute)
	if err != nil || parsed.User != nil {
		return "", fmt.Errorf("invalid page URL %q", raw)
	}
	parsed.Scheme = base.Scheme
	parsed.Host = base.Host
	parsed.Fragment = ""
	return parsed.String(), nil
}
