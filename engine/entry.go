package engine

// 本文件是**阶段入口**的声明方式。
//
// 起因：阶段原来只能在 main.go 里一个个注册（fetcher 值 + 入口回调各占一段），
// 阶段一多 main.go 就变成一堆装配代码。
// 更好的形状是**站点与阶段声明各站一个文件**（脚手架生成的 `configs/sites/<站名>.go`），
// main.go 只剩一行：
//
//	app.RegisterSites(papa.Sites()...)
//
// 那样的话"这个阶段的入口任务是什么"就不该再由 main.go 传进来 —— 让 fetcher 自己实现这个接口：
// 它是这个阶段自己的事，和 `GetStage()` 一样属于阶段的自述。

// EntrySubmitter 由 fetcher **可选**实现：本阶段"注册即投一批入口任务"（列表页入口、分类入口……）。
//
// 引擎在 ApplyRegisterStage 的第二趟调用它 —— 那时**所有**阶段的池子都已建好，
// 所以入口任务可以投给任意阶段，不必只在"自己这个阶段"上打转。
//
//	nil 回调、没实现这个接口、或实现返回后什么也不投：都是合法的，阶段照常跑（只是没有起始任务）。
type EntrySubmitter interface {
	SubmitEntries(engine *Engine)
}
