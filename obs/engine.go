package obs

import "time"

// SiteState 是报告时刻某站点的暂停快照（纯值类型，不依赖 engine 包）。
type SiteState struct {
	Key         string    `json:"key"`
	Paused      bool      `json:"paused"`
	PausedAt    time.Time `json:"paused_at,omitempty"`
	PauseSource string    `json:"pause_source,omitempty"`
	PauseReason string    `json:"pause_reason,omitempty"`
	StageCount  int       `json:"stage_count"`
}

// EngineState 是报告生成时刻的运行上下文。
type EngineState struct {
	Source         string      `json:"source"` // engine | db | events | signals | none
	CapturedAt     time.Time   `json:"captured_at"`
	Sites          []SiteState `json:"sites,omitempty"`
	ActivePools    int         `json:"active_pools"` // 已建阶段池数；0 = 空闲/暂停
	LastSnapshotAt time.Time   `json:"last_snapshot_at,omitempty"`
	Recovering     bool        `json:"recovering,omitempty"`
	Degraded       string      `json:"degraded,omitempty"`
}

// Inactive 表示「引擎现在没在跑活」：无阶段池，或全部站点暂停。
// Source=none 或 signals 时不据缺失字段判断空闲：有限信号不是完整引擎快照。
func (s EngineState) Inactive() bool {
	if s.Source == "" || s.Source == "none" || s.Source == "signals" {
		return false
	}
	if s.ActivePools == 0 {
		return true
	}
	if len(s.Sites) == 0 {
		return false
	}
	for _, site := range s.Sites {
		if !site.Paused {
			return false
		}
	}
	return true
}
