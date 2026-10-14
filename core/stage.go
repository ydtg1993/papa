package core

// 本文件是**阶段身份**的拼法：阶段名与站点名合起来才唯一。

// StageKey 一个阶段的身份文本：`site:stage`。
//
// **阶段名只在本站内唯一** —— 两个站可以各有自己的 `catalog`，所以框架内部凡是以"阶段"为键的地方
// （工作池、统计器、下载器覆盖层、日志与告警的标识）用的都是这个复合键，而不是裸的 `GetStage()` 值。
// 落盘目录也按这条切（`{dir}/{site}/{stage}/`）。
//
// 站点是必填维度（声明要有 Key、任务要有 site），所以拼出来永远是 `example:catalog` 这样两段都在。
// 与 `Task.Unique()` 的 `site:stage:url` 是同一套拼法；正因为拿它当键，
// **站点键与阶段名里都不能出现 `:`**（否则两对不同的站点/阶段会拼成同一个键，声明期就会报错）。
func StageKey(site, stage string) string {
	return site + ":" + stage
}
