package core

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// 本文件是**任务身份**：把一个页面地址规范成与站点域名无关的标识，再按框架约定
// 拼成自定义幂等键（`site:stage:<标识>`）。
//
// 为什么进框架：默认幂等键是 `site:stage:<完整 URL>`（含 host）。多镜像/换域名的站点
// 需要的是"同一页"而不是"同一个字符串" —— 但每个项目自己写一遍解析 + 拼接 + 长度校验，
// 错一个点就会让去重静默失效（重复任务悄悄入库）。

// maxIdempotencyKeyLen 与 crawler_tasks.idempotency_key 的 varchar(1000) 对齐。
// 超长时**报错而不是截断**：截断会让两个不同的 URL 撞同一个键，去重从"没生效"变成"错误生效"，
// 后者更难查。调用方应当清洗 query 或改用更短的业务标识。
const maxIdempotencyKeyLen = 1000

// PageIdentity 把页面地址规范成与站点域名无关的身份：`path[?query]`，去掉 fragment。
//
//   - 只接受绝对的 http/https 地址，且不带 userinfo；
//   - path 为空时按 `/` 处理（`https://a.com` 与 `https://a.com/` 是同一页）；
//   - path 用 **EscapedPath**（`/a%2Fb` 与 `/a/b` 是不同资源，不能被解成同一个身份）；
//   - query 原样保留（顺序有意义，V1 不做排序）。
//
// host 被忽略是有意的：租户隔离靠 TaskKey 里的 `site:stage` 前缀。这是「身份」不是「范围」。
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

// TaskKey 返回推荐的自定义幂等键：`site:stage:<PageIdentity>`。
//
// 与默认键（`site:stage:<完整 URL>`）的差别是**不含 host**：站点换域名/多镜像时仍是同一条任务。
// 它**不改变** Task.Unique 的默认行为；需要时显式赋给 Task.IdempotencyKey：
//
//	key, err := papa.TaskKey(task.Site, "detail", canonicalURL)
//	if err != nil { return err }
//	child := &papa.Task{..., IdempotencyKey: key}
//
// site / stage 任一为空即报错（缺了这两段，两个站/两个阶段会撞同一个键）。
// 键超过 crawler_tasks.idempotency_key 的 varchar(1000) 时也报错 —— 不静默截断。
func TaskKey(site, stage, rawURL string) (string, error) {
	site = strings.TrimSpace(site)
	stage = strings.TrimSpace(stage)
	if site == "" || stage == "" {
		return "", fmt.Errorf("task key needs a site and a stage")
	}
	identity, err := PageIdentity(rawURL)
	if err != nil {
		return "", err
	}
	key := site + ":" + stage + ":" + identity
	// MySQL 的 varchar(1000) 按**字符**计数，不是字节 —— 中文站点键/查询串用 len() 会误杀。
	if n := utf8.RuneCountInString(key); n > maxIdempotencyKeyLen {
		return "", fmt.Errorf("task key is %d chars (max %d; site=%q stage=%q): shorten the query string or use a short custom identifier",
			n, maxIdempotencyKeyLen, site, stage)
	}
	return key, nil
}
