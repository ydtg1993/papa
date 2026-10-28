package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// EngineSection 输出报告时刻的引擎状态：站点暂停、阶段池、治理队列、恢复进度。
type EngineSection struct {
	Querier    Querier       // 测试注入；nil 时用 in.DB
	StuckAfter time.Duration // 报告口径；0 = 10m（与 analyze.Generic 一致）
}

func (s *EngineSection) Key() string   { return "engine" }
func (s *EngineSection) Title() string { return "引擎状态" }

func (s *EngineSection) stuckAfter() time.Duration {
	if s.StuckAfter > 0 {
		return s.StuckAfter
	}
	return 10 * time.Minute
}

// requeueStat 是窗口内某治理队列的 task_requeued 聚合。
type requeueStat struct {
	Count int
	First time.Time
	Last  time.Time
}

func (s *EngineSection) Build(ctx context.Context, in *obs.Input) (*obs.SectionResult, error) {
	if in == nil {
		return nil, fmt.Errorf("nil input")
	}
	res := &obs.SectionResult{Summary: engineStateSummary(in.Engine)}
	q := s.Querier
	if q == nil && in.DB != nil {
		q = NewGormQuerier(in.DB)
	}

	// 站点表：优先用注入的实时快照，缺失时用 Querier 的 DB 快照。
	sites := in.Engine.Sites
	if len(sites) == 0 && q != nil {
		if got, err := q.SiteStates(ctx); err == nil {
			sites = got
		} else {
			in.Degraded = append(in.Degraded, "engine state: 站点快照查询失败: "+err.Error())
		}
	}
	tbl := obs.Table{Title: "站点状态", Columns: []string{"站点", "暂停", "暂停时间", "阶段数"}}
	paused := 0
	for _, st := range sites {
		if st.Paused {
			paused++
		}
		at := "-"
		if !st.PausedAt.IsZero() {
			at = fmtTime(st.PausedAt)
		}
		tbl.Rows = append(tbl.Rows, []string{st.Key, boolText(st.Paused), at, fmt.Sprintf("%d", st.StageCount)})
	}
	res.Tables = append(res.Tables, tbl)

	// 阶段池表：窗口内最后一次带 stages 的快照。
	lastStages := map[string]events.StageSnapshot{}
	for _, ev := range in.Events {
		if ev.Type == events.TypeStageSnapshot && len(ev.Stages) > 0 {
			lastStages = ev.Stages
		}
	}
	keys := make([]string, 0, len(lastStages))
	for k := range lastStages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	poolTbl := obs.Table{Title: "阶段池（窗口内最后一次快照）", Columns: []string{"阶段", "排队", "在跑", "worker"}}
	for _, k := range keys {
		st := lastStages[k]
		poolTbl.Rows = append(poolTbl.Rows, []string{k, fmt.Sprintf("%d", st.QueueLen), fmt.Sprintf("%d", st.InProgress), fmt.Sprintf("%d", st.Workers)})
	}
	res.Tables = append(res.Tables, poolTbl)

	// 恢复进度：窗口内 task_requeued 按队列计数 + 首末时间；DB 旧 processing 数。
	rstats := map[string]*requeueStat{}
	for _, ev := range in.Events {
		if ev.Type != events.TypeTaskRequeued {
			continue
		}
		name := ev.Queue
		if name == "" {
			name = "unknown"
		}
		st := rstats[name]
		if st == nil {
			st = &requeueStat{}
			rstats[name] = st
		}
		st.Count++
		if st.First.IsZero() || ev.TS.Before(st.First) {
			st.First = ev.TS
		}
		if ev.TS.After(st.Last) {
			st.Last = ev.TS
		}
	}
	var oldProcessing int64
	if q != nil {
		before := in.Now.Add(-s.stuckAfter())
		if n, err := q.OldProcessing(ctx, before); err == nil {
			oldProcessing = n
		} else {
			in.Degraded = append(in.Degraded, "engine state: 旧 processing 查询失败: "+err.Error())
		}
	}
	rt := obs.Table{Title: "恢复进度", Columns: []string{"队列", "窗口内重投", "首次", "末次"}}
	names := make([]string, 0, len(rstats))
	for k := range rstats {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		st := rstats[k]
		rt.Rows = append(rt.Rows, []string{k, fmt.Sprintf("%d", st.Count), fmtTime(st.First), fmtTime(st.Last)})
	}
	if oldProcessing > 0 {
		rt.Rows = append(rt.Rows, []string{"DB 旧 processing", fmt.Sprintf("%d", oldProcessing), "-", "-"})
	}
	res.Tables = append(res.Tables, rt)

	// Checks
	pausedCheck := obs.NewCheck("engine_paused", obs.SeverityWarn, paused == 0,
		fmt.Sprintf("%d/%d 个站点处于暂停", paused, len(sites)), "暂停期间「期末积压/卡住」不代表故障")
	res.Checks = append(res.Checks, pausedCheck)
	res.Checks = append(res.Checks,
		obs.NewCheck("stage_pools", obs.SeverityInfo, in.Engine.ActivePools > 0,
			fmt.Sprintf("活跃阶段池 %d 个", in.Engine.ActivePools), "0 = 暂停或空闲"),
		obs.NewCheck("recover_in_progress", obs.SeverityInfo, in.Engine.Recovering || oldProcessing > 0,
			fmt.Sprintf("窗口内 recover 重投 %d 次，DB 旧 processing %d 条", totalRequeued(rstats), oldProcessing),
			"启动恢复是分批的：processing 会逐批变回 pending，不代表卡死"),
		obs.NewCheck("engine_idle", obs.SeverityInfo, idle(in),
			"引擎空闲（无活跃池且窗口内无任务事件）", "空闲本身不是故障，只说明这一窗口没有产出"),
	)
	return res, nil
}

func totalRequeued(stats map[string]*requeueStat) int {
	n := 0
	for _, st := range stats {
		n += st.Count
	}
	return n
}

func engineStateSummary(st obs.EngineState) string {
	if st.Source == "" || st.Source == "none" {
		return "无运行上下文（离线报告）"
	}
	return fmt.Sprintf("来源 %s，活跃阶段池 %d，站点 %d 个", st.Source, st.ActivePools, len(st.Sites))
}

func idle(in *obs.Input) bool {
	if in.Engine.Source == "" || in.Engine.Source == "none" {
		return false
	}
	if in.Engine.ActivePools > 0 {
		return false
	}
	for _, ev := range in.Events {
		if ev.Type == events.TypeTaskFinished || ev.Type == events.TypeTaskFailed {
			return false
		}
	}
	return true
}

// fmtTime 报告里的时间口径：零值显示 "-"，否则按「秒级本地时间」输出。
// 多个分析段共用，不要在别处重复定义。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
