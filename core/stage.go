package core

// 本文件是**阶段身份**的拼法：阶段名与站点名合起来才唯一。

// StageKey 一个阶段的身份文本：`site:stage`。
//
// **阶段名只在本站内唯一** —— 两个站可以各有自己的 `catalog`，所以框架内部凡是以"阶段"为键的地方
// （工作池、统计器、下载器覆盖层、日志与告警的标识）用的都是这个复合键，而不是裸的 `GetStage()` 值。
// 落盘目录也按这条切（`{dir}/{site}/{stage}/`）。
//
// 空站点落成 `:catalog` —— 与"默认 scope"（未归属站点的阶段）以及 `Task.Unique()` 的
// `site:stage:url` 是同一套拼法：缺的那一段以空段出现，不省略分隔符。
// 所以**站点键与阶段名里都不该出现 `:`**（与幂等键的约定一致）。
func StageKey(site, stage string) string {
	return site + ":" + stage
}
