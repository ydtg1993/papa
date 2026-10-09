package engine

import "fmt"

// 本文件是**阶段依赖的启动期校验**。
//
// 问题背景：`GetFiledown()` / `GetM3U8()` 在没接线时返回 nil，而"没接线"这件事原来只在
// **第一条任务跑到那一步**才暴露出来（实测就是那句 `file downloader is configured`）——
// 那可能已经是上线几个小时、跑掉几万条任务之后了，而且失败的是任务，不是启动。
//
// 所以给 fetcher 两个**可选**接口，声明"我这个阶段的 handler 要用下载器"。
// 声明之后，`ApplyRegisterStage` 一开就检查，缺接线直接 panic —— 与配置校验层同一个取向：
// 该在启动时炸掉的东西，不要等跑到某条任务上才变成另一种行为。

// NeedsFiledown 由 fetcher 可选实现：返回 true 表示本阶段的 handler 会调
// `engine.GetFiledown()`（或自己持有的那个文件下载器）。
//
// 实现它只是让框架在启动时替你检查接线；不实现也不影响运行（那只意味着接线错了
// 要等第一条任务跑到那一步才知道）。
type NeedsFiledown interface{ NeedsFiledown() bool }

// NeedsM3U8 与 NeedsFiledown 同理，对应 `engine.GetM3U8()`。
type NeedsM3U8 interface{ NeedsM3U8() bool }

// checkStageDeps 校验一个阶段的依赖是否都已接线；不满足直接 panic（启动期，fail fast）。
//
// 报错里写清"怎么改"：这一条几乎总是"忘了在 ApplyRegisterStage 之前调 SetXxx"，
// 而 RegisterStage 的调用顺序在 main.go 里一眼能看见。
func (e *Engine) checkStageDeps(stage string, f Fetcher) {
	if d, ok := f.(NeedsFiledown); ok && d.NeedsFiledown() && e.GetFiledown() == nil {
		panic(fmt.Errorf("阶段 %s 声明依赖文件下载器（NeedsFiledown() 返回 true），但没有接线："+
			"在 ApplyRegisterStage 之前调 app.Engine.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig()))；"+
			"这个阶段其实不用下载器就删掉 fetcher 上的 NeedsFiledown()", stage))
	}
	if d, ok := f.(NeedsM3U8); ok && d.NeedsM3U8() && e.GetM3U8() == nil {
		panic(fmt.Errorf("阶段 %s 声明依赖 m3u8 下载器（NeedsM3U8() 返回 true），但没有接线："+
			"在 ApplyRegisterStage 之前调 app.Engine.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig()))；"+
			"这个阶段其实不用 m3u8 就删掉 fetcher 上的 NeedsM3U8()", stage))
	}
}
