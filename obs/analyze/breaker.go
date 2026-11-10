package analyze

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func addBreakerIncidents(in *obs.Input, res *obs.SectionResult) {
	trips := []events.Event{}
	for _, ev := range in.Events {
		if ev.Type == events.TypeBreakerTripped {
			trips = append(trips, ev)
		}
	}
	sort.SliceStable(trips, func(i, j int) bool { return trips[i].TS.Before(trips[j].TS) })
	if len(trips) > 32 {
		res.Tables = append(res.Tables, obs.Table{Title: "熔断分析范围", Columns: []string{"说明"}, Rows: [][]string{{"仅分析窗口内最近 32 次熔断"}}})
		trips = trips[len(trips)-32:]
	}
	for _, trip := range trips {
		window, _ := strconv.ParseInt(trip.Attrs["window_ns"], 10, 64)
		if window <= 0 {
			res.Problems = append(res.Problems, obs.Problem{ID: fmt.Sprintf("breaker-window-unknown-%s-%d", trip.RunID, trip.Seq), Severity: obs.SeverityWarn, Scope: trip.Site, Title: "熔断事件缺少统计窗口，无法准确关联本次失败", Evidence: []obs.Evidence{{Type: "event", Ref: fmt.Sprintf("seq=%d", trip.Seq)}}})
			continue
		}
		c := incident.Capture{ID: trip.Attrs["incident_id"], Site: trip.Site, At: trip.TS, From: trip.TS.Add(-time.Duration(window)), Window: time.Duration(window), Threshold: trip.Counts["threshold"], Failures: trip.Counts["failures"]}
		if c.ID == "" {
			c.ID = fmt.Sprintf("%s-%d", trip.RunID, trip.Seq)
		}
		if from, err := time.Parse(time.RFC3339Nano, trip.Attrs["window_from"]); err == nil && from.After(c.From) && !from.After(c.At) {
			c.From = from
		}
		c.ResumedAt, _ = time.Parse(time.RFC3339Nano, trip.Attrs["resumed_at"])
		for _, ev := range in.Events {
			if ev.Type != events.TypeTaskFailed || ev.Site != trip.Site || ev.RunID != trip.RunID || ev.TS.Before(c.From) || ev.TS.After(c.At) || (trip.Seq > 0 && ev.Seq > trip.Seq) {
				continue
			}
			s := incident.Clean(incident.Sample{At: ev.TS, TaskID: int64(ev.TaskID), Stage: ev.Stage, Kind: ev.Kind, URL: ev.URL, Error: ev.Error})
			c.Samples = append(c.Samples, s)
			if len(c.Samples) > incident.MaxSamples {
				c.Limited = true
				sort.SliceStable(c.Samples, func(i, j int) bool { return c.Samples[i].At.Before(c.Samples[j].At) })
				c.Samples = c.Samples[1:]
			}
		}
		r := incident.Analyze(c)
		table := obs.Table{Title: fmt.Sprintf("熔断 %s / %s 的相似失败", c.Site, c.At.Format(time.RFC3339)), Columns: []string{"阶段", "类别", "错误种类", "次数", "采样占比", "已知任务", "首次 / 最近", "代表任务 / URL", "脱敏错误原文"}}
		for _, group := range r.Groups {
			example := group.Examples[0]
			table.Rows = append(table.Rows, []string{group.Stage, group.Category, group.Kind, strconv.Itoa(group.Count), fmt.Sprintf("%.1f%%", group.Percent), strconv.Itoa(group.Tasks), group.First.Format(time.RFC3339) + " / " + group.Last.Format(time.RFC3339), fmt.Sprintf("%d / %s", example.TaskID, example.URL), example.Error})
		}
		res.Tables = append(res.Tables, table)
		problem := obs.Problem{ID: "breaker-analysis-" + c.ID, Severity: obs.SeverityWarn, Scope: c.Site, Count: c.Failures, Title: r.Summary, Acceptance: "修复证据所指的失败源后，新的终态失败下降且不再集中触发熔断"}
		problem.Evidence = append(problem.Evidence, obs.Evidence{Type: "event", Ref: fmt.Sprintf("run=%s seq=%d", trip.RunID, trip.Seq), Excerpt: incident.Text(r)})
		problem.Evidence = append(problem.Evidence, obs.Evidence{Type: "event", Ref: "采样覆盖", Excerpt: fmt.Sprintf("采样 %d / 触发计数 %d，数量一致且未截断：%t；占比不是请求失败率；事件缺失及窗口边界差异可能影响结论。", r.Observed, c.Failures, r.Complete)})
		for i, g := range r.Groups {
			if i >= 3 {
				break
			}
			problem.Hypotheses = append(problem.Hypotheses, obs.Hypothesis{Text: g.Category + "：" + g.Hypothesis, Confidence: "low", Verify: g.Verify})
		}
		res.Problems = append(res.Problems, problem)
	}
}
