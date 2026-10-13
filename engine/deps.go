package engine

import "fmt"

// 本文件是**阶段依赖的启动期校验**：fetcher 可选实现 `NeedsBrowser()` 声明"我这个阶段要借浏览器"，
// `ApplyRegisterStage` 一开就检查 —— 与配置校验层同一个取向：该在启动时炸掉的东西，
// 不要等跑到某条任务上才变成另一种行为。
//
// 现在只剩浏览器这一条，因为它是**唯一一个会被配置关掉**的框架自带组件（`browser.enable`，默认 false）。
// 两个下载器（文件 / m3u8）**框架一定备好基座**（`InitDownloaders`），所以"声明了下载器却没接线"
// 这件事在结构上已经不可能发生 —— 原来那两条校验（`NeedsFiledown` / `NeedsM3U8`）随之删除。

// NeedsBrowser 由 fetcher 可选实现：返回 true 表示本阶段的 handler 会借浏览器
// （`engine.GetBrowserPool()` / `FetchRendered`）。
//
// 忘了开 `browser.enable` 的表现是"每条任务跑到取浏览器实例那一步才失败"，报出来的还是一句
// 像框架自身 bug 的 `browser pool closed`。声明之后，启动那一刻就点名到配置键与并发上限。
type NeedsBrowser interface{ NeedsBrowser() bool }

// checkStageDeps 校验一个阶段的依赖是否都就绪；不满足直接 panic（启动期，fail fast）。
// 报错里写清"怎么改"（这一条几乎总是"配置里没打开那个开关"）。
// key 是**阶段键**（`site:stage`）：多站时同名阶段要能分辨是哪一个没接线。
func (e *Engine) checkStageDeps(key string, f Fetcher) {
	if d, ok := f.(NeedsBrowser); ok && d.NeedsBrowser() && e.GetBrowserPool() == nil {
		panic(fmt.Errorf("阶段 %s 声明依赖浏览器池（NeedsBrowser() 返回 true），但没有启用："+
			"在 configs/config.yaml 里把 browser.enable 打开（浏览器按需创建，并发上限是 browser.pool_size）；"+
			"这个阶段其实不用浏览器就删掉 fetcher 上的 NeedsBrowser()", key))
	}
}
