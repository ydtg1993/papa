package core

import "context"

// 本文件是**逐请求请求头**：一次抓取带上自己的头，而不动全局配置。
//
// 为什么需要它：`html.headers` / `browser.headers` 只有一套，多站同进程时"站点 A 用这套 UA、
// 站点 B 用那套 Cookie"做不到。现在两层都能带：
//
//   - **站点级**：`SiteSpec.Headers`（引擎按任务的 `Site` 自动挂上，业务不用写代码）；
//   - **单次请求**：`engine.FetchHTML(papa.WithHeaders(ctx, h), url)`。
//
// 优先级（后者覆盖前者）：框架默认 < 全局 `html.headers`/`browser.headers` < 站点 headers < 逐请求。
// 这个 ctx 键住在 core（零依赖叶子包）：静态抓取与浏览器渲染两条路都要读它，而它们都不能 import engine。

// headersCtxKey 逐请求请求头的 ctx 键。
type headersCtxKey struct{}

// WithHeaders 给这次抓取（及其派生 ctx 上的后续抓取）带上/覆盖请求头。nil 或空 map 原样返回。
//
// **同键覆盖**，值为空串表示**删掉**那个键（比如不想要框架默认的 `User-Agent`）；
// 它是**叠加**的：站点级已挂在 ctx 上时，这里只覆盖你给的那几个键，其余留着。
func WithHeaders(ctx context.Context, h map[string]string) context.Context {
	if len(h) == 0 {
		return ctx
	}
	merged := make(map[string]string, len(h))
	for k, v := range HeadersFrom(ctx) { // 已有的（站点级）先铺底
		merged[k] = v
	}
	for k, v := range h {
		merged[k] = v
	}
	return context.WithValue(ctx, headersCtxKey{}, merged)
}

// HeadersFrom 返回本次抓取要带的逐请求头；没有时返回 nil。
//
// 返回副本：调用方改它不会影响 ctx 里那份。
func HeadersFrom(ctx context.Context) map[string]string {
	h, ok := ctx.Value(headersCtxKey{}).(map[string]string)
	if !ok || len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// ApplyHeaders 把逐请求覆盖合并进 base（**原地改 base**，它必须是调用方自己造的 map）。
// 值为空串的键从 base 里删掉 —— 那是"这次不要这个头"的表达方式。
func ApplyHeaders(base map[string]string, override map[string]string) {
	for k, v := range override {
		if v == "" {
			delete(base, k)
			continue
		}
		base[k] = v
	}
}

// proxyURLKey 显式代理地址的 ctx 键。
type proxyURLKey struct{}

// WithProxyURL 指定这次抓取（及其派生 ctx 上的后续抓取）走**哪个**代理，如
// "http://1.2.3.4:8080"。空串 = 不指定，回到既有的"用不用代理"开关。
//
// 有了它，"用哪个出口"就由 fetcher 自己决定 —— 比如 `engine.GetProxy().Next()` 取一个，
// 或者站点自己有固定出口。框架只提供"取一个"和"传进去"，不替业务决定用不用。
func WithProxyURL(ctx context.Context, addr string) context.Context {
	if addr == "" {
		return ctx
	}
	return context.WithValue(ctx, proxyURLKey{}, addr)
}

// ProxyURLFrom 返回指定的代理地址；没指定时返回空串。
func ProxyURLFrom(ctx context.Context) string {
	addr, _ := ctx.Value(proxyURLKey{}).(string)
	return addr
}
