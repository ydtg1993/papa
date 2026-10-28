package obs

import (
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

// EventCoverage 是事件时间覆盖与最大断层的摘要（报告自证 + event_coverage 检查共用）。
type EventCoverage struct {
	FirstTS    time.Time     `json:"first_ts,omitempty"`
	LastTS     time.Time     `json:"last_ts,omitempty"`
	MaxGap     time.Duration `json:"max_gap,omitempty"`
	MaxGapFrom time.Time     `json:"max_gap_from,omitempty"`
	MaxGapTo   time.Time     `json:"max_gap_to,omitempty"`
}

// ComputeEventCoverage 计算首末事件与最大相邻间隔（按时间排序后比较）。
// 注意：只反映「事件文件里能看到的」，进程被硬杀导致的整段丢失不产生 seq 缺口，只能靠这里发现。
func ComputeEventCoverage(evs []events.Event) EventCoverage {
	var out EventCoverage
	sorted := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.TS.IsZero() {
			continue
		}
		sorted = append(sorted, ev)
	}
	if len(sorted) == 0 {
		return out
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })
	out.FirstTS = sorted[0].TS
	out.LastTS = sorted[len(sorted)-1].TS
	for i := 1; i < len(sorted); i++ {
		gap := sorted[i].TS.Sub(sorted[i-1].TS)
		if gap > out.MaxGap {
			out.MaxGap, out.MaxGapFrom, out.MaxGapTo = gap, sorted[i-1].TS, sorted[i].TS
		}
	}
	return out
}

// Inputs 是报告的自证信息（渲染进 report.md/report.json）。
type Inputs struct {
	SnapshotAt       time.Time         `json:"snapshot_at"`
	WindowFrom       time.Time         `json:"window_from,omitempty"`
	WindowTo         time.Time         `json:"window_to,omitempty"`
	PapaVersion      string            `json:"papa_version,omitempty"`
	PapaCommit       string            `json:"papa_commit,omitempty"`
	ConfigFile       string            `json:"config_file,omitempty"`
	Thresholds       map[string]string `json:"thresholds,omitempty"`
	ThresholdsSource string            `json:"thresholds_source,omitempty"`
	Events           EventCoverage     `json:"events"`
}
