package core

import "time"

// StageEntry 一个**阶段入口**的快照（后台「阶段入口」卡片读它：这个阶段有没有入口任务、什么模式、
// 上次手动点火是什么时候）。
//
// Mode 是**字符串**而不是引擎的枚举类型 —— core 是零依赖叶子包，反过来 import engine 就成环了；
// 边界上 `EntryMode.String()` 出来就是 "auto" / "manual"。
// 时间用零值表示"从未"，与 `SiteStat.BreakerPausedAt` / `QueueStat` 同一约定
// （前端 `fmtAgo` 会把 0001 年判成从未）。
type StageEntry struct {
	Site  string `json:"site"`
	Stage string `json:"stage"`
	// Mode 入口模式：`auto` = 启动时已经投过（后台只展示，不给点火按钮）；
	// `manual` = 等后台手动点火。
	Mode string `json:"mode"`
	// LastTriggeredAt 上一次**手动点火**的时刻（零值 = 从未）；启动时自动投的那一次不算。
	LastTriggeredAt time.Time `json:"last_triggered_at"`
	// Paused 这个阶段现在停不停 —— **含继承**：整站被暂停时，它下面的每个阶段都算停。
	// 后台那一行据此画「暂停/恢复」，所以它必须与闸门看到的一致（同一个判定）。
	Paused bool `json:"paused"`
	// PauseSource 暂停是谁下的（`manual` / `breaker`）；没暂停时为空。
	PauseSource string `json:"pause_source"`
}
