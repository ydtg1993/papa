// Package analyze 是 papa 报告引擎的内置分析器：只消费通用事件与框架自身状态
// （crawler_tasks 等），不含任何业务维度。业务分析以 obs.Section 注册进来。
package analyze

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	// CrawlFailRateMax 抓取失败事件率上限（事件尝试口径：分母=窗口内 task_finished+task_failed）。
	CrawlFailRateMax float64
	// EventMaxGap 事件流允许的最大相邻间隔：超过即认为存在时间断层（丢事件/进程被硬杀）。
	EventMaxGap time.Duration
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

func (t Thresholds) eventMaxGap() time.Duration {
	if t.EventMaxGap > 0 {
		return t.EventMaxGap
	}
	return 10 * time.Minute
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
	// 停机区间只还原一次，运行边界与事件覆盖共用：两处对「窗口里哪段是停机」必须是同一口径。
	dts := obs.ComputeDowntimes(in.Events, reportWindowTo(in))
	g.addRuntime(in, res, dts)
	addEventCoverage(in, res, g.Thresholds.eventMaxGap(), dts)
	g.addQueue(in, res)
	groups := g.addFailures(in, res)
	g.addPerformance(in, res)
	g.addFrameworkState(ctx, in, res)
	// 失败分组数只算 task_failed 的分组，不用 len(res.Problems)：
	// 后者把 run-boundary / no-stage-snapshot 等框架问题也算成「失败分组」，会虚高。
	res.Summary = fmt.Sprintf("事件 %d 条，失败分组 %d 个", len(in.Events), groups)
	return res, nil
}

/* ---------- 运行边界 ---------- */

func (g *Generic) addRuntime(in *obs.Input, res *obs.SectionResult, dts []obs.Downtime) {
	var started, stopped int
	for _, ev := range in.Events {
		switch ev.Type {
		case events.TypeRunStarted:
			started++
		case events.TypeRunStopped:
			stopped++
		}
	}
	rows := [][]string{
		{"事件条数", fmt.Sprintf("%d", len(in.Events))},
		{"运行启动次数", fmt.Sprintf("%d", started)},
		{"运行结束事件", fmt.Sprintf("%d", stopped)},
	}
	// 只统计有事件依据的运行区间（含暂停），已识别停机另列。
	if effective, downtime, ok := windowRuntime(in, dts); ok {
		value := fmtSpan(effective)
		if downtime > 0 {
			value += "（已识别停机 " + fmtSpan(downtime) + "）"
		}
		rows = append(rows, []string{"有事件依据的运行时长（含暂停）", value})
	} else {
		rows = append(rows, []string{"有事件依据的运行时长（含暂停）", "-"})
	}
	rows = append(rows,
		[]string{"事件文件数", fmt.Sprintf("%d", in.EventMeta.Files)},
		[]string{"坏行数", fmt.Sprintf("%d", in.EventMeta.Unparsed)},
		[]string{"重复事件", fmt.Sprintf("%d", in.EventMeta.Duplicates)},
		[]string{"seq 缺口", fmt.Sprintf("%d", in.EventMeta.Gaps)},
		[]string{"日志文件数", fmt.Sprintf("%d", in.Logs.Files)},
		[]string{"日志坏行", fmt.Sprintf("%d", in.Logs.Unparsed)},
	)
	res.Tables = append(res.Tables, obs.Table{
		Title:   "运行边界",
		Columns: []string{"项", "值"},
		Rows:    rows,
	})
	res.Checks = append(res.Checks,
		obs.NewCheck("event_gaps", obs.SeverityWarn, in.EventMeta.Gaps == 0,
			fmt.Sprintf("事件 seq 缺口 %d 个", in.EventMeta.Gaps),
			"缺口 = 丢弃（buffer 满/写失败）或轮转丢行；事件不完整时以 DB 为准"),
		obs.NewCheck("log_unparsed", obs.SeverityInfo, in.Logs.Unparsed == 0,
			fmt.Sprintf("日志坏行 %d 条", in.Logs.Unparsed), "只影响证据索引，不影响判定"),
	)
	// 窗口里有事件但既没有 run_started、也收集不到任何 run_id：可能是从轮转尾部读取。
	// 有 run_id 说明 run 起于窗口前（常驻进程跨天），是正常现象，不报问题。
	if started == 0 && len(in.Run.RunIDs) == 0 && len(in.Events) > 0 {
		res.Problems = append(res.Problems, obs.Problem{
			ID: "run-boundary-missing", Severity: obs.SeverityWarn,
			Title: "事件里没有 run_started（可能从轮转尾部读取）", Count: 1,
			Acceptance: "能解释窗口起点；否则放宽 --since 重跑报告",
		})
	}
	// 窗口内多次启动 = 进程重启，由 pattern.Builtin() 的 process-restarts 报，避免重复。
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
	var lastSpill *events.Event // 窗口内最后一条 spill_changed：溢出表（引擎内存）现状
	var latestTS time.Time      // 最近一次**含 stages** 的快照时间：queue_drained 的新鲜度判定
	hasSnapshotEvent := false
	for i := range in.Events {
		ev := &in.Events[i]
		switch ev.Type {
		case events.TypeStageSnapshot:
			hasSnapshotEvent = true
			latestTS = ev.TS
			latest = ev.Stages // 完整快照，空值表示池已退出，不能保留上次队列。

			lastQueues = ev.Queues
		case events.TypeSpillChanged:
			lastSpill = ev
		}
	}
	if len(latest) == 0 {
		// 暂停/空闲时「期末队列必须清空」不适用：明确跳过，而不是报红。
		if engineInactive(in) {
			c := obs.NewCheck("queue_drained", obs.SeverityWarn, false,
				"引擎暂停/无活跃阶段，队列不可采样", "暂停快照，不适用")
			c.Skipped, c.SkipReason = true, "引擎暂停/无活跃阶段，队列无法采样"
			res.Checks = append(res.Checks, c)
			return
		}
		// 只有「窗口内完全没有 stage_snapshot 事件」才提示检查配置；
		// 有事件但 stages 全空 = 引擎没建池（暂停/空闲），不是配置错。
		if !hasSnapshotEvent {
			res.Problems = append(res.Problems, obs.Problem{
				ID: "no-stage-snapshot", Severity: obs.SeverityWarn,
				Title: "没有 stage_snapshot 事件（报告无法给队列时序）", Count: 1,
				Acceptance: "确认 obs.events.snapshot_interval > 0 且事件流已开启",
			})
		}
		return
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// InProgress = submitted-completed-failed，**已包含还在队列里排队的**（workerpool.Stats），
	// 所以这里只累加 InProgress，不能再加 QueueLen（旧实现两列相加，排队被算了两遍）。
	tbl := obs.Table{Title: "队列快照（窗口内最后一次）", Columns: []string{"阶段键", "排队", "未完成（含排队）", "worker", "完成", "失败", "提交"}}
	var outstanding int
	for _, k := range keys {
		s := latest[k]
		tbl.Rows = append(tbl.Rows, []string{
			k, fmt.Sprintf("%d", s.QueueLen), fmt.Sprintf("%d", s.InProgress), fmt.Sprintf("%d", s.Workers),
			fmt.Sprintf("%d", s.Completed), fmt.Sprintf("%d", s.Failed), fmt.Sprintf("%d", s.Submitted),
		})
		outstanding += s.InProgress
	}
	res.Tables = append(res.Tables, tbl)

	govBacklog := 0
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
			govBacklog += int(q.Backlog)
		}
		res.Tables = append(res.Tables, qt)
	}

	// 溢出表（spill）是引擎内存里被高水位挤出来、等待每 drain_interval 回灌的任务：
	// 它们不在内存队列里（QueueLen 看不到），但确实是没跑完的存量，单列一张小表并计入队列积压。
	spillBacklog := 0
	if lastSpill != nil {
		spillBacklog = int(lastSpill.Backlog)
		st := obs.Table{Title: "溢出表（spill，引擎内存）", Columns: []string{"最近时刻", "最近回灌", "当前积压"}}
		st.Rows = append(st.Rows, []string{
			fmtTime(lastSpill.TS), fmt.Sprintf("%d", lastSpill.Counts["drained"]), fmt.Sprintf("%d", spillBacklog),
		})
		res.Tables = append(res.Tables, st)
	}

	backlog := outstanding + govBacklog
	if spillBacklog > 0 {
		backlog += spillBacklog
	}
	if engineInactive(in) {
		c := obs.NewCheck("queue_drained", obs.SeverityWarn, false, "", "")
		c.Skipped, c.SkipReason = true, "引擎暂停/无活跃阶段，队列无法采样"
		res.Checks = append(res.Checks, c)
	} else {
		stale := !latestTS.IsZero() && in.Now.Sub(latestTS) > g.Thresholds.stuckAfter()
		msg := fmt.Sprintf("窗口末仍有 %d 条未完成/积压（未完成 %d、治理 %d、spill %d）",
			backlog, outstanding, govBacklog, spillBacklog)
		if backlog == 0 {
			msg = "窗口末队列已收敛（未完成 0）"
		}
		if stale {
			msg += fmt.Sprintf("（快照时间 %s，已陈旧）", latestTS.Format("15:04:05"))
		}
		severity := obs.SeverityWarn
		if in.Mode == obs.ModeSnapshot {
			severity = obs.SeverityInfo
		}
		res.Checks = append(res.Checks, obs.NewCheck("queue_drained", severity, backlog == 0, msg,
			"有界测试窗口结束时应为 0；常驻进程应按当前负载判断；未完成=InProgress（已含排队）"))
	}
}

/* ---------- 失败归因 ---------- */

// failTaskStat 是失败组内某个 task_id 的失败次数与首次失败时间。
type failTaskStat struct {
	First time.Time
	Count int
}

type failGroup struct {
	Site, Stage, Kind string
	Count             int
	FirstTS, LastTS   time.Time
	// Attempts 按 attempt 统计次数；Errors 按归一化模板（errorTemplate）统计次数。
	Attempts map[int]int
	Errors   map[string]int
	// Tasks 按 task_id 去重统计：证据里要能看出「几个任务失败」而不是「首条任务失败几次」。
	Tasks map[uint]*failTaskStat

	SampleTask uint
	SampleURL  string
	SampleMsg  string
}

// addFailures 把 task_failed 按 site × stage × kind 分组落成 Problem，返回失败分组数。
func (g *Generic) addFailures(in *obs.Input, res *obs.SectionResult) int {
	groups := map[string]*failGroup{}
	for _, ev := range in.Events {
		if ev.Type != events.TypeTaskFailed {
			continue
		}
		key := ev.Site + "|" + ev.Stage + "|" + ev.Kind
		grp := groups[key]
		if grp == nil {
			grp = &failGroup{
				Site: ev.Site, Stage: ev.Stage, Kind: ev.Kind,
				Attempts: map[int]int{}, Errors: map[string]int{}, Tasks: map[uint]*failTaskStat{},
			}
			groups[key] = grp
		}
		grp.Count++
		if grp.FirstTS.IsZero() || ev.TS.Before(grp.FirstTS) {
			grp.FirstTS = ev.TS
		}
		if grp.LastTS.IsZero() || ev.TS.After(grp.LastTS) {
			grp.LastTS = ev.TS
		}
		grp.Attempts[ev.Attempt]++
		if ev.TaskID != 0 {
			ts := grp.Tasks[ev.TaskID]
			if ts == nil {
				ts = &failTaskStat{First: ev.TS}
				grp.Tasks[ev.TaskID] = ts
			} else if ev.TS.Before(ts.First) {
				ts.First = ev.TS
			}
			ts.Count++
		}
		if tpl := errorTemplate(ev.Error); tpl != "" {
			grp.Errors[tpl]++
		}
		if grp.SampleTask == 0 && ev.TaskID != 0 {
			grp.SampleTask = ev.TaskID
			grp.SampleURL = ev.URL
			grp.SampleMsg = ev.Error
		}
	}
	if len(groups) == 0 {
		return 0
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
		topTpl := topTemplate(grp.Errors)
		prob := obs.Problem{
			ID:       "task-failed-" + sanitizeID(grp.Site) + "-" + sanitizeID(grp.Stage) + "-" + sanitizeID(grp.Kind),
			Severity: obs.SeverityWarn,
			Title:    fmt.Sprintf("%s/%s 终态失败 %d 次（kind=%s）", grp.Site, grp.Stage, grp.Count, grp.Kind),
			Scope:    grp.Site + "/" + grp.Stage,
			Count:    grp.Count,
			Evidence: []obs.Evidence{{
				Type:    "event",
				Ref:     failureTasksRef(grp),
				Excerpt: truncate(grp.SampleMsg, 500),
			}},
		}
		if topTpl != "" {
			prob.Evidence = append(prob.Evidence, obs.Evidence{
				Type: "event", Ref: "错误模板", Excerpt: topTpl,
			})
		}
		prob.Hypotheses = hypothesesForKind(grp.Kind, grp.SampleMsg)
		if sample, ok := findLogSampleForGroup(in, grp); ok {
			prob.Evidence = append(prob.Evidence, obs.Evidence{
				Type: "log", Ref: fmt.Sprintf("%s:%d", sample.File, sample.Line),
				Excerpt: truncate(sample.Msg, 500),
			})
		}
		prob.Acceptance = "重跑该阶段，同 kind 的失败数下降或归零"
		res.Problems = append(res.Problems, prob)
	}
	return len(groups)
}

// failureTasksRef 把失败组的事件证据写成「按 task_id 去重的失败任务清单」。
//
// 旧实现用「组内首条 task_id + 组首末时间 + 组次数」拼成一句「task_id=7（…~…，共N次）」，
// 一个组里有两个不同任务时会被读成「同一个任务失败了 N 次」，证据是错的
// （2026-10-29 报告：hgdju/catalog 组实际是 task 9676 与 9678 各 1 次，却写成 9676 共 2 次）。
func failureTasksRef(grp *failGroup) string {
	ids := make([]uint, 0, len(grp.Tasks))
	for id := range grp.Tasks {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return fmt.Sprintf("失败事件 %d 次（事件未带 task_id）", grp.Count)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// 组很大时只列前 5 个任务，避免证据行无限长；总数与总次数仍然给全。
	const maxList = 5
	parts := make([]string, 0, maxList+1)
	for i, id := range ids {
		if i >= maxList {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%d@%s", id, fmtTime(grp.Tasks[id].First)))
	}
	return fmt.Sprintf("失败任务 %d 个：%s（共 %d 次）", len(ids), strings.Join(parts, "、"), grp.Count)
}

// errNumRE / errURLRE 在包级编译一次：errorTemplate 对每条失败事件都会调用。
var (
	errNumRE = regexp.MustCompile(`\d+`)
	errURLRE = regexp.MustCompile(`https?://\S+`)
)

// errorTemplate 把错误文本归一成模板（去 task failed: 前缀 / 数字 -> # / URL -> <url>），
// 用于同组内 TopN：一串带 task_id、行号、URL 的错误文本只有归一后才能看出是同一个错。
func errorTemplate(msg string) string {
	s := msg
	if i := strings.Index(s, "task failed:"); i >= 0 {
		s = s[i+len("task failed:"):]
	}
	s = errNumRE.ReplaceAllString(s, "#")
	s = errURLRE.ReplaceAllString(s, "<url>")
	// 按字符（rune）截断，避免把多字节字符切成半个。
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return strings.TrimSpace(s)
}

// topTemplate 返回组内出现次数最多的错误模板；并列时取字典序最小者，保证报告可复现。
func topTemplate(counts map[string]int) string {
	best, bestN := "", 0
	for tpl, n := range counts {
		if n > bestN || n == bestN && tpl < best {
			best, bestN = tpl, n
		}
	}
	return best
}

// findLogSampleForGroup 先按本组的 task_id / URL / site+stage 找日志样本，
// 找不到才回退到日志里第一条 "task failed"。
// 否则所有失败组都会引用同一行日志（往往是别的组的证据），证据并不成立。
func findLogSampleForGroup(in *obs.Input, grp *failGroup) (obs.LogSample, bool) {
	needles := []string{}
	if grp.SampleTask != 0 {
		needles = append(needles, fmt.Sprintf("task_id=%d", grp.SampleTask))
	}
	if grp.SampleURL != "" {
		needles = append(needles, grp.SampleURL)
	}
	if grp.Site != "" && grp.Stage != "" {
		needles = append(needles, grp.Site+" "+grp.Stage)
	}
	for _, needle := range needles {
		for _, sig := range in.Logs.Signatures {
			for _, s := range sig.Samples {
				if strings.Contains(s.Msg, needle) {
					return s, true
				}
			}
		}
	}
	return findLogSample(in, "task failed")
}

// hypothesesForKind 先用错误文本给高置信度假设，认不出错误文本时回退到按 kind 的通用假设。
func hypothesesForKind(kind, errText string) []obs.Hypothesis {
	switch {
	case strings.Contains(errText, "Access is denied"), strings.Contains(errText, "browser binary"),
		strings.Contains(errText, "debug url"), strings.Contains(errText, "ProcessSingleton"):
		return []obs.Hypothesis{{
			Text:       "浏览器/文件系统权限问题（rod 下载目录或 Chrome user-data-dir 不可写）",
			Confidence: "high", Verify: "`papa rod --self-check` 与检查 rod 目录 ACL",
			FixHint: "browser.binary / user-data-dir 指向可写目录",
		}}
	case strings.Contains(errText, "unsupported detail route"), strings.Contains(errText, "grid missing"):
		return []obs.Hypothesis{{
			Text: "站点页面结构变化或 URL 规则不符", Confidence: "high",
			Verify: "用 `papa rod --url <失败 URL>` 对照归档页面", FixHint: "站点声明/抓取解析规则",
		}}
	default:
		return hypothesesFor(kind)
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
			fmt.Sprintf("抓取失败事件率 %.1f%%（%d/%d 次尝试）", rate*100, failCount, totalCount),
			fmt.Sprintf("事件尝试口径：分母=窗口内 task_finished+task_failed，含后续被重投的失败；终态失败（按任务行）看任务终态分布。阈值 ≤ %.0f%%（未校准，首轮后按真实数据调整）",
				g.Thresholds.crawlFailRateMax()*100)))
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
	stuckDetail := "报告口径，不是引擎的启动恢复判据（后者是 updated_at < startedAt）"
	if !hasEngineContext(in) {
		stuckDetail += "（无引擎上下文，无法区分暂停与卡住）"
	}
	stuckCheck := obs.NewCheck("stuck_processing", obs.SeverityFail, stuck == 0,
		fmt.Sprintf("processing 超过 %s 未更新：%d 条", g.Thresholds.stuckAfter(), stuck), stuckDetail)
	if engineInactive(in) {
		stuckCheck.Skipped = true
		stuckCheck.SkipReason = "引擎暂停/无活跃阶段，processing 不代表卡住"
	}
	if in.Mode == obs.ModeSnapshot {
		// 快照回答「现在怎样」：常驻进程跑批中途 processing 非 0 是常态，不能按验收口径判 fail
		//（2026-10-29 报告就是因为这类 check 在 snapshot 下给了 fail；prepare/push settled 已先这么做）。
		stuckCheck.Severity = obs.SeverityInfo
		stuckCheck.Detail += "（仅表示长时间未更新；未确认执行者失活或恢复失败，不据此认定故障）"
	}
	res.Checks = append(res.Checks, stuckCheck)

	var unfinished int64
	if err := in.DB.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("status IN ? AND repeatable = ?", []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}, models.RepeatableNo).
		Count(&unfinished).Error; err != nil {
		in.Degraded = append(in.Degraded, "framework state: 存留检查查询失败: "+err.Error())
		return
	}
	unfinishedDetail := "有界测试窗口结束时应为 0；轮询任务不计入"
	if !hasEngineContext(in) {
		unfinishedDetail += "（无引擎上下文，无法区分暂停与卡住）"
	}
	unfinishedCheck := obs.NewCheck("unfinished_tasks", obs.SeverityFail, unfinished == 0,
		fmt.Sprintf("非轮询任务期末仍有 %d 条 pending/processing", unfinished), unfinishedDetail)
	if engineInactive(in) {
		unfinishedCheck.Skipped = true
		unfinishedCheck.SkipReason = "暂停快照，期末=0 语义不适用"
	}
	if in.Mode == obs.ModeSnapshot {
		// 同上：常驻进程的 pending 队列是「正在排队」，不是「这轮没跑完」。
		unfinishedCheck.Severity = obs.SeverityInfo
		unfinishedCheck.Message = fmt.Sprintf("当前非轮询任务有 %d 条 pending/processing", unfinished)
		unfinishedCheck.Detail = "当前待处理进度，轮询任务不计入，不作为异常"
	}
	res.Checks = append(res.Checks, unfinishedCheck)

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

// reportWindowTo 返回报告窗口上界：Run.To 优先；没有窗口（单点报告）用报告时刻。
func reportWindowTo(in *obs.Input) time.Time {
	if !in.Run.To.IsZero() && (in.Now.IsZero() || in.Run.To.Before(in.Now)) {
		return in.Run.To
	}
	return in.Now
}

// windowRuntime 返回窗口内有事件依据的运行时长（含暂停）与已识别停机时长。
// 没有窗口（From 为零）时 ok=false，报告里显示 "-"。
func windowRuntime(in *obs.Input, dts []obs.Downtime) (effective, downtime time.Duration, ok bool) {
	from, to := in.Run.From, reportWindowTo(in)
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return 0, 0, false
	}
	effective = obs.ObservedRuntime(in.Events, from, to)
	downtime = obs.TotalDowntime(dts, from, to)
	return effective, downtime, true
}

// fmtSpan 把时长按「秒级」渲染（报告不展示纳秒误差）；零/负值给 "0s"。
func fmtSpan(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// engineInactive 判断引擎是否处于暂停/空闲：暂停/空闲时「期末积压/卡住」不是故障。
// 暂停时不要求收敛；验收模式也不能把正常暂停当作失活。
func engineInactive(in *obs.Input) bool {
	return in != nil && in.Engine.Inactive()
}

// hasEngineContext 报告是否拿到了运行时的引擎上下文。
// Source 为空或 none 表示离线/无 DB：此时不跳过，只在 detail 里注明无法区分暂停与卡住。
func hasEngineContext(in *obs.Input) bool {
	return in != nil && in.Engine.Source != "" && in.Engine.Source != "none"
}

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
