package browser

import (
	"fmt"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// BlockOptions 只作用于这一页的资源拦截。
//
// 两类规则的实现不同，代价也不同：
//
//   - URLPatterns 走 Network.setBlockedURLs：浏览器网络层直接失败，不经过 Go，开销最小；
//   - ResourceTypes 走 Fetch 域（rod 的 HijackRequests）：每个请求会经过 Go 一层，
//     并且会关闭页面缓存。能写 URL 模式就别按资源类型拦。
type BlockOptions struct {
	// URLPatterns 按 URL 通配拦截（CDP 的 * ? 语法），如 "*google-analytics*"、"*.woff2"。
	URLPatterns []string

	// ResourceTypes 按资源类型拦截（Image / Media / Font / Stylesheet …）。
	ResourceTypes []proto.NetworkResourceType
}

// applyBlock 在**导航前**把本页的拦截规则挂上（newPageBase 的调用点保证时序）。
//
// 两类规则同时配置时都生效；失败一律返回 error —— 不能静默降级成"没拦住"，
// 调用方会以为页面已经不加载图片/统计脚本了。
func applyBlock(page *rod.Page, opts *BlockOptions) error {
	if opts == nil {
		return nil
	}
	if len(opts.URLPatterns) > 0 {
		// setBlockedURLs 只在 Network 域启用后生效（Puppeteer 也是这个顺序）。
		if err := (proto.NetworkEnable{}).Call(page); err != nil {
			return fmt.Errorf("enable network domain: %w", err)
		}
		if err := (proto.NetworkSetBlockedURLs{Urls: opts.URLPatterns}).Call(page); err != nil {
			return fmt.Errorf("set blocked urls: %w", err)
		}
	}
	if len(opts.ResourceTypes) > 0 {
		router := page.HijackRequests()
		for _, typ := range opts.ResourceTypes {
			// 每种类型挂一条 "*" 规则：Fetch.enable 的 pattern 自带 ResourceType，
			// 浏览器只会暂停这一类的请求。
			if err := router.Add("*", typ, func(h *rod.Hijack) {
				h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			}); err != nil {
				return fmt.Errorf("block resource type %s: %w", typ, err)
			}
		}
		// Run 阻塞消费 Fetch 事件；页面关闭时 page 的 ctx 取消，它会自己退出。
		go router.Run()
	}
	return nil
}
