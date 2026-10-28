// Package analyze 是 papa 报告引擎的内置分析器：只消费通用事件与框架自身状态
// （crawler_tasks 等），不含任何业务维度。业务分析以 obs.Section 注册进来。
package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// Thresholds 是通用判定阈值；零值取默认（可在后续版本接配置）。
type Thresholds struct {
	// StuckAfter 判"卡住"的阈值：processing 超过它仍未更新即视为卡住。
	// 这是**报告口径**，不是引擎启动恢复判据（后者是 updated_at < startedAt）。
	StuckAfter time.Duration
	// CrawlFailRateMax 抓取终态失败率上限。
	CrawlFailRateMax float64
}

func (t Thresholds) stuckAfter() time.Duration {
	if t.StuckAfter > 0 {
		return t.StuckAfter
	}
	return 10 * time.Minute
}

func (t Thresholds) crawlFailRateMax() float64 {
	if t.CrawlFailRateMax > 0 {
		return t.CrawlFailRateMax
	}
	return 0.05
}

// Generic 是框架内置的通用分析节：运行边界、队列健康、失败归因、性能、框架状态。
//
// 所有输出只用 site/stage/kind 这类通用维度；业务维度由业务 Section 负责。
type Generic struct {
	Thresholds Thresholds
}

func (g *Generic) Key() string   { return "framework" }
func (g *Generic) Title() string { return "框架运行概览" }

func (g *Generic) Build(ctx context.Context, in *obs.Input) (*obs.SectionResult, error) {
	if in == nil {
		return nil, fmt.Errorf("nil input")
	}
	res := &obs.SectionResult{}
	g.addRuntime(in, res)
	g.addQueue(in, res)
	g.addFailures(in, res)
	g.addPerformance(in, res)
	g.addFrameworkState(ctx, in, res)
	if res.Summary == "" {
		res.Summary = fmt.Sprintf("事件 %d 条，失败分组 %d 个", len(in.Events), len(res.Problems))
	}
	return res, nil
}

/* ---------- 运行边界 ---------- */

func (g *Generic) addRuntime(in *obs.Input, res *obs.SectionResult) {
	var started, stopped int
	for _, ev := range in.Events {
		switch ev.Type {
		case events.TypeRunStarted:
			started++
		case events.TypeRunStopped:
			stopped++
		}
	}
	res.Tables = append(res.Tables, obs.Table{
		Title:   "运行边界",
		Columns: []string{"项", "值"},
		Rows: [][]string{
			{"事件条数", fmt.Sprintf("%d", len(in.Events))},
			{"运行启动次数", fmt.Sprintf("%d", started)},
			{"运行结束事件", fmt.Sprintf("%d", stopped)},
			{"事件文件数", fmt.Sprintf("%d", in.EventMeta.Files)},
			{"坏行数", fmt.Sprintf("%d", in.EventMeta.Unparsed)},
			{"重复事件", fmt.Sprintf("%d", in.EventMeta.Duplicates)},
			{"seq 缺口", fmt.Sprintf("%d", in.EventMeta.Gaps)},
			{"日志文件数", fmt.Sprintf("%d", in.Logs.Files)},
			{"日志坏行", fmt.Sprintf("%d", in.Logs.Unparsed)},
		},
	})
	res.Checks = append(res.Checks,
		obs.NewCheck("event_gaps", obs.SeverityWarn, in.EventMeta.Gaps == 0,
			fmt.Sprintf("事件 seq 缺口 %d 个", in.EventMeta.Gaps),
			"缺口 = 丢弃（buffer 满/写失败）或轮转丢行；事件不完整时以 DB 为准"),
		obs.NewCheck("log_unparsed", obs.SeverityInfo, in.Logs.Unparsed == 0,
			fmt.Sprintf("日志坏行 %d 条", in.Logs.Unparsed), "只影响证据索引，不影响判定"),
	)
	if started == 0 && len(in.Events) > 0 {
		res.Problems = append(res.Problems, obs.Problem{
			ID: "run-boundary-missing", Severity: obs.SeverityWarn,
			Title: "事件里没有 run_started（可能从轮转尾部读取）", Count: 1,
			Acceptance: "能解释窗口起点；否则放宽 --since 重跑报告",
		})
	}
	if started > 1 {
		res.Problems = append(res.Problems, obs.Problem{
			ID: "process-restarts", Severity: obs.SeverityWarn,
			Title: fmt.Sprintf("窗口内进程启动 %d 次（含重启）", started), Count: started,
			Acceptance: "确认重启不是由崩溃引起；对照 run_stopped.outcome",
		})
	}
}

/* ---------- 队列健康 ---------- */

type stageRow struct {
	Key        string
	QueueLen   int
	InProgress int
	Workers    int
	Submitted  int64
	Completed  int64
	Failed     int64
}

func (g *Generic) addQueue(in *obs.Input, res *obs.SectionResult) {
	latest := map[string]events.StageSnapshot{}
	var lastQueues map[string]events.QueueSnapshot
	for _, ev := range in.Events {
		if ev.Type != events.TypeStageSnapshot || len(ev.Stages) == 0 {
			continue
		}
		for k, v := range ev.Stages {
			latest[k] = v
		}
		lastQueues = ev.Queues
	}
	if len(latest) == 0 {
		res.Problems = append(res.Problems, obs.Problem{
			ID: "no-stage-snapshot", Severity: obs.SeverityWarn,
			Title: "没有 stage_snapshot 事件（报告无法给队列时序）", Count: 1,
			Acceptance: "确认 obs.events.snapshot_interval > 0 且事件流已开启",
		})
		return
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tbl := obs.Table{Title: "队列快照（窗口内最后一次）", Columns: []string{"阶段键", "排队", "在跑", "worker", "完成", "失败", "提交"}}
	var backlog int
	for _, k := range keys {
		s := latest[k]
		tbl.Rows = append(tbl.Rows, []string{
			k, fmt.Sprintf("%d", s.QueueLen), fmt.Sprintf("%d", s.InProgress), fmt.Sprintf("%d", s.Workers),
			fmt.Sprintf("%d", s.Completed), fmt.Sprintf("%d", s.Failed), fmt.Sprintf("%d", s.Submitted),
		})
		backlog += s.QueueLen + s.InProgress
	}
	res.Tables = append(res.Tables, tbl)

	if lastQueues != nil {
		qt := obs.Table{Title: "治理队列（窗口内最后一次）", Columns: []string{"队列", "启用", "运行中", "积压", "累计处理", "上次耗时ms", "上次错误"}}
		qkeys := make([]string, 0, len(lastQueues))
		for k := range lastQueues {
			qkeys = append(qkeys, k)
		}
		sort.Strings(qkeys)
		for _, k := range qkeys {
			q := lastQueues[k]
			qt.Rows = append(qt.Rows, []string{
				k, boolText(q.Enabled), boolText(q.Running), fmt.Sprintf("%d", q.Backlog),
				fmt.Sprintf("%d", q.TotalProcessed), fmt.Sprintf("%d", q.LastDurationMS), q.LastError,
			})
			backlog += int(q.Backlog)
		}
		res.Tables = append(res.Tables, qt)
	}
	res.Checks = append(res.Checks, obs.NewCheck("queue_drained", obs.SeverityWarn, backlog == 0,
		fmt.Sprintf("窗口末队列仍有 %d 条排队/在跑/积压", backlog),
		"有界测试窗口结束时应为 0；常驻进程应按当前负载判断"))
}

/* ---------- 失败归因 ---------- */

type failGroup struct {
	Site, Stage, Kind string
	Count             int
	SampleTask        uint
	SampleURL         string
	SampleMsg         string
}

func (g *Generic) addFailures(in *obs.Input, res *obs.SectionResult) {
	groups := map[string]*failGroup{}
	for _, ev := range in.Events {
		if ev.Type != events.TypeTaskFailed {
			continue
		}
		key := ev.Site + "|" + ev.Stage + "|" + ev.Kind
		grp := groups[key]
		if grp == nil {
			grp = &failGroup{Site: ev.Site, Stage: ev.Stage, Kind: ev.Kind}
			groups[key] = grp
		}
		grp.Count++
		if grp.SampleTask == 0 && ev.TaskID != 0 {
			grp.SampleTask = ev.TaskID
			grp.SampleURL = ev.URL
			grp.SampleMsg = ev.Error
		}
	}
	if len(groups) == 0 {
		return
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tbl := obs.Table{Title: "失败分组（site × stage × kind）", Columns: []string{"站点", "阶段", "种类", "次数", "样本任务", "样本 URL"}}
	for _, k := range keys {
		grp := groups[k]
		tbl.Rows = append(tbl.Rows, []string{
			grp.Site, grp.Stage, grp.Kind, fmt.Sprintf("%d", grp.Count),
			fmt.Sprintf("%d", grp.SampleTask), truncate(grp.SampleURL, 80),
		})
	}
	res.Tables = append(res.Tables, tbl)

	for _, k := range keys {
		grp := groups[k]
		prob := obs.Problem{
			ID:       "task-failed-" + sanitizeID(grp.Site) + "-" + sanitizeID(grp.Stage) + "-" + sanitizeID(grp.Kind),
			Severity: obs.SeverityWarn,
			Title:    fmt.Sprintf("%s/%s 终态失败 %d 次（kind=%s）", grp.Site, grp.Stage, grp.Count, grp.Kind),
			Scope:    grp.Site + "/" + grp.Stage,
			Count:    grp.Count,
			Evidence: []obs.Evidence{{
				Type: "event", Ref: fmt.Sprintf("task_failed task_id=%d", grp.SampleTask),
				Excerpt: truncate(grp.SampleMsg, 500),
			}},
		}
		prob.Hypotheses = hypothesesFor(grp.Kind)
		if sample, ok := findLogSample(in, "task failed"); ok {
			prob.Evidence = append(prob.Evidence, obs.Evidence{
				Type: "log", Ref: fmt.Sprintf("%s:%d", sample.File, sample.Line),
				Excerpt: truncate(sample.Msg, 500),
			})
		}
		prob.Acceptance = "重跑该阶段，同 kind 的失败数下降或归零"
		res.Problems = append(res.Problems, prob)
	}
}

// hypothesesFor 按通用错误种类给可验证假设（不含任何业务文件路径）。
func hypothesesFor(kind string) []obs.Hypothesis {
	switch kind {
	case "structure", "not-found", "not_found":
		return []obs.Hypothesis{{
			Text: "目标页面结构变化或 URL 失效，选择器/链接规则与真实页面不符", Confidence: "high",
			Verify:  "用 `papa rod --url <失败 URL>` 抓一次真实页面，对照归档页面与选择器",
			FixHint: "该阶段 fetcher 的解析逻辑与站点声明（sites/<站>.go）",
		}}
	case "protected", "access_restricted":
		return []obs.Hypothesis{{
			Text: "被反爬/登录墙/地区限制拦截", Confidence: "medium",
			Verify:  "`papa rod --url <URL> --show` 人工看页面是否要求登录/验证码",
			FixHint: "站点请求头、Cookie/代理配置或抓取频率",
		}}
	default:
		return []obs.Hypothesis{{
			Text: "网络抖动、浏览器资源不足或目标站限流", Confidence: "medium",
			Verify:  "`papa rod --self-check` 验浏览器可用；对照失败时间分布是否集中",
			FixHint: "browser.pool_size / 阶段并发与重试退避",
		}}
	}
}

func findLogSample(in *obs.Input, contains string) (obs.LogSample, bool) {
	for _, sig := range in.Logs.Signatures {
		for _, s := range sig.Samples {
			if contains != "" && !containsFold(s.Msg, contains) {
				continue
			}
			return s, true
		}
	}
	return obs.LogSample{}, false
}

/* ---------- 性能 ---------- */

func (g *Generic) addPerformance(in *obs.Input, res *obs.SectionResult) {
	type acc struct {
		Count int
		Durs  []int64
	}
	byStage := map[string]*acc{}
	var failCount, totalCount int
	for _, ev := range in.Events {
		switch ev.Type {
		case events.TypeTaskFinished, events.TypeTaskFailed:
			totalCount++
			if ev.Type == events.TypeTaskFailed {
				failCount++
			}
			if ev.DurationMS <= 0 {
				continue
			}
			key := ev.Site + "/" + ev.Stage
			a := byStage[key]
			if a == nil {
				a = &acc{}
				byStage[key] = a
			}
			a.Count++
			a.Durs = append(a.Durs, ev.DurationMS)
		}
	}
	if totalCount > 0 {
		rate := float64(failCount) / float64(totalCount)
		res.Checks = append(res.Checks, obs.NewCheck("crawl_fail_rate", obs.SeverityWarn,
			rate <= g.Thresholds.crawlFailRateMax(),
			fmt.Sprintf("抓取终态失败率 %.1f%%（%d/%d）", rate*100, failCount, totalCount),
			fmt.Sprintf("阈值 ≤ %.0f%%（未校准，首轮后按真实数据调整）", g.Thresholds.crawlFailRateMax()*100)))
	}
	if len(byStage) == 0 {
		return
	}
	keys := make([]string, 0, len(byStage))
	for k := range byStage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tbl := obs.Table{Title: "任务耗时（事件口径，窗口内）", Columns: []string{"站点/阶段", "样本数", "p50 ms", "p95 ms", "max ms"}}
	for _, k := range keys {
		a := byStage[k]
		sort.Slice(a.Durs, func(i, j int) bool { return a.Durs[i] < a.Durs[j] })
		tbl.Rows = append(tbl.Rows, []string{
			k, fmt.Sprintf("%d", a.Count),
			fmt.Sprintf("%d", percentile(a.Durs, 0.50)),
			fmt.Sprintf("%d", percentile(a.Durs, 0.95)),
			fmt.Sprintf("%d", a.Durs[len(a.Durs)-1]),
		})
	}
	res.Tables = append(res.Tables, tbl)
}

/* ---------- 框架状态（DB） ---------- */

func (g *Generic) addFrameworkState(ctx context.Context, in *obs.Input, res *obs.SectionResult) {
	if in.DB == nil {
		in.Degraded = append(in.Degraded, "framework state: 未配置数据库，跳过卡住/存留检查")
		return
	}
	var stuck int64
	cutoff := in.Now.Add(-g.Thresholds.stuckAfter())
	if err := in.DB.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("status = ? AND updated_at < ?", models.TaskStatusProcessing, cutoff).
		Count(&stuck).Error; err != nil {
		in.Degraded = append(in.Degraded, "framework state: 卡住检查查询失败: "+err.Error())
		return
	}
	res.Checks = append(res.Checks, obs.NewCheck("stuck_processing", obs.SeverityFail, stuck == 0,
		fmt.Sprintf("processing 超过 %s 未更新：%d 条", g.Thresholds.stuckAfter(), stuck),
		"报告口径，不是引擎的启动恢复判据（后者是 updated_at < startedAt）"))

	var unfinished int64
	if err := in.DB.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("status IN ? AND repeatable = ?", []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}, models.RepeatableNo).
		Count(&unfinished).Error; err != nil {
		in.Degraded = append(in.Degraded, "framework state: 存留检查查询失败: "+err.Error())
		return
	}
	res.Checks = append(res.Checks, obs.NewCheck("unfinished_tasks", obs.SeverityFail, unfinished == 0,
		fmt.Sprintf("非轮询任务期末仍有 %d 条 pending/processing", unfinished),
		"有界测试窗口结束时应为 0；轮询任务不计入"))

	var total, failed, done int64
	type row struct {
		Status models.TaskStatus
		N      int64
	}
	var rows []row
	if err := in.DB.WithContext(ctx).Model(&models.CrawlerTask{}).
		Select("status, COUNT(*) AS n").Group("status").Scan(&rows).Error; err == nil {
		tbl := obs.Table{Title: "任务终态分布（crawler_tasks 当前值）", Columns: []string{"状态", "数量"}}
		for _, r := range rows {
			total += r.N
			switch r.Status {
			case models.TaskStatusSuccess:
				done = r.N
			case models.TaskStatusFailed:
				failed = r.N
			}
			tbl.Rows = append(tbl.Rows, []string{statusText(r.Status), fmt.Sprintf("%d", r.N)})
		}
		if len(tbl.Rows) > 0 {
			res.Tables = append(res.Tables, tbl)
		}
		_ = done
		_ = failed
		_ = total
	} else {
		in.Degraded = append(in.Degraded, "framework state: 状态分布查询失败: "+err.Error())
	}
}

/* ---------- 小工具 ---------- */

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func boolText(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func statusText(s models.TaskStatus) string {
	switch s {
	case models.TaskStatusPending:
		return "待处理"
	case models.TaskStatusProcessing:
		return "处理中"
	case models.TaskStatusSuccess:
		return "成功"
	case models.TaskStatusFailed:
		return "失败"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if equalFold(s[i:i+n], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func sanitizeID(s string) string {
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
