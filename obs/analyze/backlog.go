package analyze

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// BacklogSection 输出积压分布、老化分桶、窗口吞吐与 ETA。
type BacklogSection struct {
	Querier Querier // 测试注入；nil 时用 in.DB
}

func (s *BacklogSection) Key() string   { return "backlog" }
func (s *BacklogSection) Title() string { return "积压与老化（含 ETA）" }

func (s *BacklogSection) Build(ctx context.Context, in *obs.Input) (*obs.SectionResult, error) {
	if in == nil {
		return nil, fmt.Errorf("nil input")
	}
	res := &obs.SectionResult{}
	q := s.Querier
	if q == nil && in.DB != nil {
		q = NewGormQuerier(in.DB)
	}
	if q == nil {
		in.Degraded = append(in.Degraded, "backlog: 未连接数据库，积压/ETA 跳过")
		res.Summary = "未连接数据库，积压/ETA 跳过"
		return res, nil
	}
	buckets, err := q.TaskBuckets(ctx, in.Now)
	if err != nil {
		return nil, fmt.Errorf("积压查询: %w", err)
	}

	// 表 1：积压分布
	dist := obs.Table{Title: "积压分布（crawler_tasks）", Columns: []string{"站点", "阶段", "状态", "轮询", "年龄", "数量"}}
	sort.Slice(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.Status != b.Status {
			return a.Status < b.Status
		}
		return a.AgeBucket < b.AgeBucket
	})
	var totalPending, totalProcessing, nonRepeatablePending int64
	for _, b := range buckets {
		switch b.Status {
		case 0:
			totalPending += b.N
			if b.Repeatable == 0 {
				nonRepeatablePending += b.N
			}
		case 1:
			totalProcessing += b.N
		}
		repeat := "否"
		if b.Repeatable != 0 {
			repeat = "是"
		}
		dist.Rows = append(dist.Rows, []string{b.Site, b.Stage, taskStatusText(b.Status), repeat, b.AgeBucket, fmt.Sprintf("%d", b.N)})
	}
	res.Tables = append(res.Tables, dist)

	// 吞吐：窗口内 task_finished + task_failed 按 site/stage 计数。
	perf := map[string]int64{}
	for _, ev := range in.Events {
		if ev.Type != events.TypeTaskFinished && ev.Type != events.TypeTaskFailed {
			continue
		}
		perf[ev.Site+"/"+ev.Stage]++
	}
	// 分母用**有效运行时长** = 窗口 - 停机区间。进程不在的时段没有事件，把它算进吞吐
	// 会按比例压低速率、放大 ETA（2026-10-29 报告就是 9h47m 停机把 1.38/min 低报到真实
	// 值的约三分之一）。停机区间由 obs.ComputeDowntimes 从 run_stopped↔run_started 还原。
	to := in.Run.To
	if to.IsZero() {
		to = in.Now
	}
	window := to.Sub(in.Run.From)
	if in.Run.From.IsZero() || window <= 0 {
		window = time.Minute
	}
	dts := obs.ComputeDowntimes(in.Events, to)
	downtime := obs.TotalDowntime(dts, in.Run.From, to)
	effective := window - downtime
	if effective <= 0 {
		effective = window
	}
	effectiveMin := effective.Minutes()
	pendingByStage := map[string]int64{}
	for _, b := range buckets {
		if b.Status == 0 && b.Repeatable == 0 {
			pendingByStage[b.Site+"/"+b.Stage] += b.N
		}
	}

	idle := in.Engine.Inactive()
	etaTbl := obs.Table{Title: "吞吐与 ETA", Columns: []string{"站点/阶段", "窗口完成数", "吞吐(条/分)", "非轮询待处理", "ETA"}}
	keys := make([]string, 0, len(pendingByStage))
	for k := range pendingByStage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var totalRate float64
	for _, k := range keys {
		n := perf[k]
		rate := float64(n) / effectiveMin
		totalRate += rate
		eta := "-"
		if rate > 0 && !idle {
			eta = fmtEta(time.Duration(float64(pendingByStage[k])/rate) * time.Minute)
		} else if idle {
			eta = "ETA 不适用（引擎未运行）"
		}
		etaTbl.Rows = append(etaTbl.Rows, []string{k, fmt.Sprintf("%d", n), fmt.Sprintf("%.2f", rate), fmt.Sprintf("%d", pendingByStage[k]), eta})
	}
	res.Tables = append(res.Tables, etaTbl)

	// 摘要 ETA 用整体口径：非轮询待处理总量 ÷ 总吞吐。per-stage 的 ETA 仍逐行保留，
	// 但整体不能再用「循环里最后一个」——那只是排序最后一行的 ETA，既不是最大者也不是总量口径。
	etaText := ""
	if totalRate > 0 && !idle {
		etaText = fmtEta(time.Duration(float64(nonRepeatablePending)/totalRate) * time.Minute)
	}

	res.Metrics = obs.BacklogSummary{
		TotalPending: totalPending, TotalProcessing: totalProcessing,
		NonRepeatablePending: nonRepeatablePending, ThroughputPerMin: totalRate, ETA: etaText,
	}
	runText := "有效运行 " + fmtSpan(effective)
	if downtime > 0 {
		runText += "（剔除停机 " + fmtSpan(downtime) + "）"
	}
	res.Summary = fmt.Sprintf("积压 %d 条（其中非轮询待处理 %d、处理中 %d），吞吐 %.2f 条/分（%s），预计 %s",
		totalPending+totalProcessing, nonRepeatablePending, totalProcessing, totalRate, runText, orDash(etaText))

	res.Checks = append(res.Checks,
		obs.NewCheck("backlog_present", obs.SeverityInfo, true,
			fmt.Sprintf("非终态任务 %d 条", totalPending+totalProcessing), "报告时刻全库快照，不是窗口末"),
	)
	// throughput_zero 的文案必须跟着判定走：通过时写吞吐实况、失败时才写「0 吞吐」，
	// 否则报告会出现「✅ 通过 | 窗口内 0 吞吐」这种自相矛盾（旧实现无条件写失败句式）。
	zeroPassed := !(nonRepeatablePending > 0 && totalRate == 0)
	zeroMsg := ""
	switch {
	case totalRate > 0:
		zeroMsg = fmt.Sprintf("窗口内吞吐 %.2f 条/分（非轮询待处理 %d 条）", totalRate, nonRepeatablePending)
	case nonRepeatablePending > 0:
		zeroMsg = fmt.Sprintf("有 %d 条非轮询待处理但窗口内 0 吞吐", nonRepeatablePending)
	default:
		zeroMsg = "窗口内 0 吞吐（无非轮询待处理）"
	}
	zero := obs.NewCheck("throughput_zero", obs.SeverityWarn, zeroPassed, zeroMsg, "引擎在跑但没吃任务，通常是 worker/队列问题")
	if idle {
		zero.Skipped, zero.SkipReason = true, "引擎暂停/空闲，ETA 与吞吐不适用"
	}
	res.Checks = append(res.Checks, zero)
	return res, nil
}

func taskStatusText(status int) string {
	switch status {
	case 0:
		return "待处理"
	case 1:
		return "处理中"
	case 2:
		return "成功"
	case 3:
		return "失败"
	default:
		return fmt.Sprintf("未知(%d)", status)
	}
}

func fmtEta(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("约 %.0f 天", math.Ceil(d.Hours()/24))
	}
	return fmt.Sprintf("约 %.1f 小时", math.Ceil(d.Hours()*10)/10)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
