package obs

import (
	"sort"
	"strconv"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

func isHistoricalWindow(run RunMeta, now time.Time) bool {
	if run.To.IsZero() {
		return false
	}
	end := run.To.In(now.Location())
	return end.Format("2006-01-02") < now.Format("2006-01-02")
}

// 历史事件仍可判失败，当前库的状态只能展示，不能冒充历史期末。
func skipHistoricalStateChecks(key string, res *SectionResult) {
	ids := map[string]bool{"stuck_processing": true, "unfinished_tasks": true, "throughput_zero": true,
		"series_funnel_rate": true, "episodes_funnel_rate": true, "prepare_settled": true, "push_settled": true,
		"push_record_missing": true, "push_hash_stale": true, "snapshot_missing": true, "engine_paused": true}
	for i := range res.Checks {
		if ids[res.Checks[i].ID] {
			res.Checks[i].Skipped = true
			res.Checks[i].SkipReason = "当前数据库/引擎快照，不能用于历史窗口判定"
		}
	}
	if key == "business_funnel" {
		res.Problems = nil
	}
}

// ObservedRuntime 只累计各run首末可见事件之间的区间，不把日历窗口的空白当作运行。
// 这包含暂停，不能用于宣称抓取有效工作时间；缺失事件时只代表可观测时长。
func ObservedRuntime(evs []events.Event, from, to time.Time) time.Duration {
	type span struct{ start, end time.Time }
	runs := map[string]span{}
	anonymous := 0
	ordered := append([]events.Event(nil), evs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TS.Before(ordered[j].TS) })
	for _, ev := range ordered {
		if ev.TS.IsZero() || (!from.IsZero() && ev.TS.Before(from)) || (!to.IsZero() && ev.TS.After(to)) {
			continue
		}
		key := ev.RunID
		if key == "" {
			if ev.Type == events.TypeRunStarted {
				anonymous++
			}
			key = "anonymous-" + strconv.Itoa(anonymous)
		}
		s := runs[key]
		if s.start.IsZero() || ev.TS.Before(s.start) {
			s.start = ev.TS
		}
		if ev.TS.After(s.end) {
			s.end = ev.TS
		}
		runs[key] = s
	}
	spans := make([]span, 0, len(runs))
	for _, s := range runs {
		spans = append(spans, s)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	var total time.Duration
	var last span
	for _, s := range spans {
		if last.start.IsZero() {
			last = s
			continue
		}
		if !s.start.After(last.end) {
			if s.end.After(last.end) {
				last.end = s.end
			}
			continue
		}
		total += last.end.Sub(last.start)
		last = s
	}
	if !last.start.IsZero() {
		total += last.end.Sub(last.start)
	}
	return total
}
