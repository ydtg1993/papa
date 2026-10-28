package report

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ydtg1993/papa/v3/obs"
)

// AIBrief 是交给另一个 AI 的排查任务包（ai-brief.json 的根）。
//
// 与 report.json 的分工：report.json 面向"指标与判定"，AIBrief 面向"问题与行动" ——
// 自带边界、已知陷阱、可验证假设与验收标准，让接手者不必重新摸索口径。
type AIBrief struct {
	SchemaVersion int `json:"schema_version"`
	// GeneratedAt 与 report.json 的 generated_at 同源：这份排查包是什么时候生成的（最近一次更新）。
	GeneratedAt   string            `json:"generated_at,omitempty"`
	Run           AIBriefRun        `json:"run"`
	Verdict       string            `json:"verdict"`
	Mode          string            `json:"mode,omitempty"`
	VerdictReason string            `json:"verdict_reason,omitempty"`
	Environment   map[string]string `json:"environment,omitempty"`
	Degraded      []string          `json:"degraded,omitempty"`
	Checks        []obs.Check       `json:"checks"`
	Problems      []obs.Problem     `json:"problems"`
	Boundaries    []string          `json:"boundaries"`
	KnownPitfalls []string          `json:"known_pitfalls"`
	Unknowns      []string          `json:"unknowns,omitempty"`
	NextActions   []string          `json:"next_actions,omitempty"`
	// Snapshot 是「现在怎样」的摘要：暂停站点/活跃阶段池/积压与 ETA，供接手者一眼看清当前状态。
	Snapshot AIBriefSnapshot `json:"snapshot,omitempty"`
}

// AIBriefSnapshot 是快照摘要的机读形态（ai-brief.md 的「现在怎样」段由它渲染）。
type AIBriefSnapshot struct {
	Mode          string             `json:"mode,omitempty"`
	VerdictReason string             `json:"verdict_reason,omitempty"`
	PausedSites   []string           `json:"paused_sites,omitempty"`
	ActivePools   int                `json:"active_pools"`
	Backlog       obs.BacklogSummary `json:"backlog,omitempty"`
}

// hasContent 判断快照摘要是否值得渲染：全空的快照不占版面（历史报告没有 mode/engine/backlog）。
func (s AIBriefSnapshot) hasContent() bool {
	return s.Mode != "" || s.VerdictReason != "" || len(s.PausedSites) > 0 ||
		s.Backlog != (obs.BacklogSummary{})
}

// AIBriefRun 是任务包里的运行标识（时间用字符串，便于外部工具直接消费）。
type AIBriefRun struct {
	RunIDs    []string `json:"run_ids,omitempty"`
	From      string   `json:"from,omitempty"`
	To        string   `json:"to,omitempty"`
	Events    int      `json:"events"`
	EventGaps int      `json:"event_gaps"`
}

// 通用已知陷阱（框架级，业务陷阱由业务问题里的假设携带）。
var genericPitfalls = []string{
	"updated_at 会被 claim/重试/终态反复覆盖，不能用来算单次耗时；耗时以事件 duration_ms 或 crawler_task_trace 为准。",
	"recover_queue 只在进程启动时跑一次；运行期 processing 超时不是重投依据（那会与仍在跑的 worker 撞车）。",
	"同一次失败会写两条日志（engine 的 task failed + worker 转发），聚合时按 task_id+retry 去重。",
	"stuck 阈值（默认 10m）是报告口径，不是引擎的启动恢复判据（后者是 updated_at < startedAt）。",
	"日志时间是本地时间且无时区；事件 ts 带时区。跨源对齐时以事件/DB 为准。",
	"事件 seq 出现缺口 = 有事件被丢弃或丢失；状态以 DB 为权威，事件用于时间线。",
}

// 只读边界（报告只给建议，不自动执行）。
var briefBoundaries = []string{
	"本任务包只读：不要写业务表、不要重投任务、不要调用接收端。",
	"不要修改既有表结构与迁移；修改代码前先确认改动范围与回滚方式。",
	"修复后必须重跑对应阶段，并用同一命令重新生成报告复验（见 next_actions 的验收项）。",
}

// BuildAIBrief 从报告生成 AI 排查任务包（确定性：不引入随机/当前时间）。
func BuildAIBrief(rep *obs.Report) *AIBrief {
	if rep == nil {
		return &AIBrief{SchemaVersion: obs.SchemaVersion, Verdict: string(obs.VerdictPass)}
	}
	out := &AIBrief{
		SchemaVersion: obs.SchemaVersion,
		GeneratedAt:   fmtTime(rep.GeneratedAt),
		Run: AIBriefRun{
			RunIDs:    rep.Run.RunIDs,
			From:      fmtTime(rep.Run.From),
			To:        fmtTime(rep.Run.To),
			Events:    rep.EventMeta.Count,
			EventGaps: rep.EventMeta.Gaps,
		},
		Verdict:       string(rep.Verdict),
		Mode:          string(rep.Mode),
		VerdictReason: rep.VerdictReason,
		Environment:   rep.Environment,
		Degraded:      rep.Degraded,
		Checks:        rep.Checks,
		Problems:      rep.Problems,
		Boundaries:    briefBoundaries,
		KnownPitfalls: genericPitfalls,
	}
	out.Snapshot.Mode = string(rep.Mode)
	out.Snapshot.VerdictReason = rep.VerdictReason
	out.Snapshot.ActivePools = rep.Engine.ActivePools
	for _, site := range rep.Engine.Sites {
		if site.Paused {
			out.Snapshot.PausedSites = append(out.Snapshot.PausedSites, site.Key)
		}
	}
	// 积压段是 batch2 新增段；没有该段（或 Metrics 不是约定类型）时留零值，渲染层会跳过。
	for _, sec := range rep.Sections {
		if sec.Key != "backlog" {
			continue
		}
		if m, ok := sec.Metrics.(obs.BacklogSummary); ok {
			out.Snapshot.Backlog = m
		}
	}
	for _, p := range rep.Problems {
		action := fmt.Sprintf("%s：按假设验证后修复", p.ID)
		if p.Acceptance != "" {
			action += "；验收：" + p.Acceptance
		}
		out.NextActions = append(out.NextActions, action)
		if len(out.NextActions) >= 5 {
			break
		}
	}
	for _, c := range rep.Checks {
		if !c.Skipped {
			continue
		}
		reason := c.SkipReason
		if reason == "" {
			reason = "判定项标记为不适用"
		}
		out.Unknowns = append(out.Unknowns, fmt.Sprintf("`%s` 本次未判定：%s", c.ID, reason))
	}
	if len(rep.Degraded) > 0 {
		out.Unknowns = append(out.Unknowns,
			"报告存在降级项，部分结论可能不完整：先补数据源再下结论（见 degraded）")
	}
	out.Unknowns = append(out.Unknowns,
		"日志文案可能随版本变化：证据引用以 file:line 的原文为准",
		"外部依赖（接收端/第三方服务）的行为需按真实失败分组核实，报告只给假设")
	return out
}

// AIBriefJSON 渲染 ai-brief.json（缩进、稳定）。
func AIBriefJSON(rep *obs.Report) ([]byte, error) {
	return json.MarshalIndent(BuildAIBrief(rep), "", "  ")
}

// AIBriefMarkdown 渲染 ai-brief.md（从同一 JSON 结构生成，保证两份一致）。
func AIBriefMarkdown(rep *obs.Report) string {
	b := BuildAIBrief(rep)
	var s strings.Builder
	fmt.Fprintf(&s, "# AI 排查任务包 · %s\n\n", strings.Join(b.Run.RunIDs, ", "))
	fmt.Fprintf(&s, "**判定：%s**（%s）\n\n", VerdictLabel(obs.Verdict(b.Verdict)), b.Verdict)
	fmt.Fprintf(&s, "- 模式：%s\n", modeText(obs.Mode(b.Mode)))
	if b.VerdictReason != "" {
		fmt.Fprintf(&s, "- 判定原因：%s\n", b.VerdictReason)
	}
	fmt.Fprintf(&s, "- 窗口：%s ~ %s\n- 事件：%d 条 · seq 缺口 %d\n- 生成时间：%s（最近一次更新）\n\n",
		b.Run.From, b.Run.To, b.Run.Events, b.Run.EventGaps, b.GeneratedAt)

	if len(b.Environment) > 0 {
		s.WriteString("## 环境事实\n\n")
		keys := make([]string, 0, len(b.Environment))
		for k := range b.Environment {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			fmt.Fprintf(&s, "- `%s`: %s\n", k, b.Environment[k])
		}
		s.WriteString("\n")
	}

	// 「现在怎样」：模式与判定原因在头部已有一行，这里只给快照独有的事实（暂停/阶段池/积压），
	// 避免同一份简报把判定信息重复两遍。
	if b.Snapshot.hasContent() {
		s.WriteString("## 现在怎样（快照摘要）\n\n")
		if len(b.Snapshot.PausedSites) > 0 {
			fmt.Fprintf(&s, "- 暂停站点：%s\n", strings.Join(b.Snapshot.PausedSites, "、"))
		}
		fmt.Fprintf(&s, "- 活跃阶段池：%d\n", b.Snapshot.ActivePools)
		if b.Snapshot.Backlog != (obs.BacklogSummary{}) {
			fmt.Fprintf(&s, "- 积压：%s\n", backlogText(b.Snapshot.Backlog))
		}
		s.WriteString("\n")
	}

	s.WriteString("## 边界（先读）\n\n")
	for _, x := range b.Boundaries {
		fmt.Fprintf(&s, "- %s\n", x)
	}
	s.WriteString("\n")

	s.WriteString("## 问题清单（按影响排序）\n\n")
	if len(b.Problems) == 0 {
		s.WriteString("无。\n\n")
	}
	for i, p := range b.Problems {
		fmt.Fprintf(&s, "### P%d %s\n\n", i+1, p.Title)
		fmt.Fprintf(&s, "- 级别：%s", p.Severity)
		if p.Scope != "" {
			fmt.Fprintf(&s, " · 范围：%s", p.Scope)
		}
		if p.Count > 0 {
			fmt.Fprintf(&s, " · 次数：%d", p.Count)
		}
		s.WriteString("\n")
		for _, ev := range p.Evidence {
			fmt.Fprintf(&s, "- 证据（%s）：`%s`", ev.Type, ev.Ref)
			if ev.Excerpt != "" {
				fmt.Fprintf(&s, " — %s", oneLine(ev.Excerpt, 300))
			}
			s.WriteString("\n")
		}
		for _, h := range p.Hypotheses {
			fmt.Fprintf(&s, "- 假设（%s）：%s", h.Confidence, h.Text)
			if h.Verify != "" {
				fmt.Fprintf(&s, " · 验证：%s", h.Verify)
			}
			if h.FixHint != "" {
				fmt.Fprintf(&s, " · 修复方向：%s", h.FixHint)
			}
			s.WriteString("\n")
		}
		if p.Acceptance != "" {
			fmt.Fprintf(&s, "- 验收：%s\n", p.Acceptance)
		}
		s.WriteString("\n")
	}

	s.WriteString("## 已知陷阱\n\n")
	for _, x := range b.KnownPitfalls {
		fmt.Fprintf(&s, "- %s\n", x)
	}
	s.WriteString("\n")

	if len(b.Unknowns) > 0 {
		s.WriteString("## 未知项（需运行时确认）\n\n")
		for _, x := range b.Unknowns {
			fmt.Fprintf(&s, "- %s\n", x)
		}
		s.WriteString("\n")
	}
	if len(b.NextActions) > 0 {
		s.WriteString("## 下一步动作\n\n")
		for _, x := range b.NextActions {
			fmt.Fprintf(&s, "- %s\n", x)
		}
		s.WriteString("\n")
	}
	return s.String()
}

// backlogText 把积压摘要渲染成一行：核心数字始终给，非轮询/吞吐/ETA 有值才给（零值跳过）。
func backlogText(bl obs.BacklogSummary) string {
	parts := []string{
		fmt.Sprintf("pending %d", bl.TotalPending),
		fmt.Sprintf("processing %d", bl.TotalProcessing),
	}
	if bl.NonRepeatablePending > 0 {
		parts = append(parts, fmt.Sprintf("非轮询 %d", bl.NonRepeatablePending))
	}
	if bl.ThroughputPerMin > 0 {
		parts = append(parts, fmt.Sprintf("吞吐 %.1f/min", bl.ThroughputPerMin))
	}
	if bl.ETA != "" {
		parts = append(parts, "ETA "+bl.ETA)
	}
	return strings.Join(parts, " · ")
}

// sortStrings 是小工具（避免为一次排序引入更多依赖）。
func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
