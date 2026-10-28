package obs

import "time"

// SiteState 是报告时刻某站点的暂停快照（纯值类型，不依赖 engine 包）。
type SiteState struct {
	Key        string    `json:"key"`
	Paused     bool      `json:"paused"`
	PausedAt   time.Time `json:"paused_at,omitempty"`
	StageCount int       `json:"stage_count"`
}

// EngineState 是报告生成时刻的运行上下文。
type EngineState struct {
	Source         string      `json:"source"` // engine | db | events | none
	CapturedAt     time.Time   `json:"captured_at"`
	Sites          []SiteState `json:"sites,omitempty"`
	ActivePools    int         `json:"active_pools"` // 已建阶段池数；0 = 空闲/暂停
	LastSnapshotAt time.Time   `json:"last_snapshot_at,omitempty"`
	Recovering     bool        `json:"recovering,omitempty"`
	Degraded       string      `json:"degraded,omitempty"`
}

// Inactive 表示「引擎现在没在跑活」：无阶段池，或全部站点暂停。
// Source=none（离线且无 DB）时刻意返回 false：没有依据时不做跳过。
func (s EngineState) Inactive() bool {
	if s.Source == "" || s.Source == "none" {
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
