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

// Downtime 是一次「进程不在运行」的区间：run_stopped 之后到下一次 run_started 之前。
//
// 停机期间没有事件是正常的，不能当成事件丢失；报告用它对 event_coverage 与吞吐/ETA 做剔除。
type Downtime struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// ComputeDowntimes 从事件流还原停机区间：把 run_stopped 与它之后第一次 run_started 配对；
// 末尾未闭合的 run_stopped 一直算到 to（窗口末/报告时刻）。
//
// 只认事件里能看到的边界：如果进程在窗口开始前就已经停机（窗口里只有 run_started、没有
// run_stopped），这里还原不出那段缺口，仍交给 event_coverage 按普通断层处理——宁可报可疑，
// 也不要凭「窗口前可能停过」去猜。
func ComputeDowntimes(evs []events.Event, to time.Time) []Downtime {
	sorted := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.TS.IsZero() {
			continue
		}
		sorted = append(sorted, ev)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })

	var out []Downtime
	var stoppedAt time.Time
	for _, ev := range sorted {
		switch ev.Type {
		case events.TypeRunStopped:
			// 同一段停机里重复的 run_stopped（理论不该有）只认第一次，避免把区间切碎。
			if stoppedAt.IsZero() {
				stoppedAt = ev.TS
			}
		case events.TypeRunStarted:
			if stoppedAt.IsZero() {
				continue
			}
			if ev.TS.After(stoppedAt) {
				out = append(out, Downtime{From: stoppedAt, To: ev.TS})
			}
			stoppedAt = time.Time{}
		}
	}
	if !stoppedAt.IsZero() && to.After(stoppedAt) {
		out = append(out, Downtime{From: stoppedAt, To: to})
	}
	return out
}

// TotalDowntime 把停机区间按 [from,to] 裁剪后累加；from/to 零值表示该侧不裁剪。
// 报告窗口外发生的停机不该从窗口里扣，所以调用方应把窗口边界传进来。
func TotalDowntime(dts []Downtime, from, to time.Time) time.Duration {
	var total time.Duration
	for _, dt := range dts {
		start, end := dt.From, dt.To
		if !from.IsZero() && start.Before(from) {
			start = from
		}
		if !to.IsZero() && end.After(to) {
			end = to
		}
		if end.After(start) {
			total += end.Sub(start)
		}
	}
	return total
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
