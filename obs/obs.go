// Package obs 是 papa 的运行观测报告引擎。
//
// 分工：papa 只定义**通用**的输入与产物 —— 事件流（pkg/events）、日志证据（LogSummary）、
// 框架自身状态（crawler_tasks 等）与判定模型（Check/Verdict）。业务维度（预处理、推送、
// 数据一致性……）通过 Section / Pattern 注册进来，本包不认识任何业务语义。
package obs

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
	"gorm.io/gorm"
)

// SchemaVersion 是报告 JSON（report.json / ai-brief.json）的 schema 版本；只增不改。
const SchemaVersion = 1

// Severity 判定项与问题的严重级。
type Severity string

const (
	SeverityFail Severity = "fail"
	SeverityWarn Severity = "warn"
	SeverityInfo Severity = "info"
)

// Verdict 一次运行的总体结论。
type Verdict string

const (
	VerdictPass    Verdict = "pass"    // 全部通过
	VerdictWarn    Verdict = "warn"    // 带风险通过（只有警告未通过）
	VerdictFail    Verdict = "fail"    // 不通过（至少一条 fail 未通过）
	VerdictUnknown Verdict = "unknown" // 未判定：数据不足/暂停快照，无法下结论
)

// Check 一个判定项。papa 只认 ID/Severity/Passed/Message，不解释其业务语义。
type Check struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Passed   bool     `json:"passed"`
	// Skipped 本次不适用：不参与判定；SkipReason 说明为什么（暂停/无阶段/数据缺失）。
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"`
}

// Evidence 一条证据引用（事件 / 日志 / 数据库）。
type Evidence struct {
	Type    string `json:"type"`              // event / log / db
	Ref     string `json:"ref"`               // events.jsonl#seq=12 / engine.log:88 / crawler_tasks id=3
	Excerpt string `json:"excerpt,omitempty"` // 截断后的原文
}

// Hypothesis 一条可验证的假设（AI 细则的行动依据）。
type Hypothesis struct {
	Text       string `json:"text"`
	Confidence string `json:"confidence"`         // high / medium / low
	Verify     string `json:"verify,omitempty"`   // 验证命令或步骤
	FixHint    string `json:"fix_hint,omitempty"` // 修复锚点（file:line 或方向）
}

// Problem 一条可行动问题（进人读报告与 AI 细则）。
type Problem struct {
	ID         string       `json:"id"`
	Severity   Severity     `json:"severity"`
	Title      string       `json:"title"`
	Scope      string       `json:"scope,omitempty"`
	Count      int          `json:"count,omitempty"`
	Evidence   []Evidence   `json:"evidence,omitempty"`
	Hypotheses []Hypothesis `json:"hypotheses,omitempty"`
	Acceptance string       `json:"acceptance,omitempty"`
	Origin     string       `json:"origin,omitempty"` // section | pattern | check
}

// Table 人读表格。
type Table struct {
	Title   string     `json:"title,omitempty"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// SectionResult 一节的分析结果。
type SectionResult struct {
	Summary  string    `json:"summary,omitempty"`
	Metrics  any       `json:"metrics,omitempty"`
	Checks   []Check   `json:"checks,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
	Tables   []Table   `json:"tables,omitempty"`
}

// BacklogSummary 是积压段的机读摘要（供 ai-brief 的「现在怎样」快照复用）。
type BacklogSummary struct {
	TotalPending         int64   `json:"total_pending"`
	TotalProcessing      int64   `json:"total_processing"`
	NonRepeatablePending int64   `json:"non_repeatable_pending"`
	ThroughputPerMin     float64 `json:"throughput_per_min"`
	ETA                  string  `json:"eta,omitempty"`
}

// Section 由框架内置分析器或业务实现，向报告贡献一节。
type Section interface {
	Key() string
	Title() string
	Build(ctx context.Context, in *Input) (*SectionResult, error)
}

// Pattern 把一组证据映射成可行动问题。
type Pattern interface {
	Key() string
	Match(in *Input) []Problem
}

// RunMeta 一次运行的身份与时间窗口。
type RunMeta struct {
	RunIDs []string  `json:"run_ids,omitempty"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

// EventMeta 事件读取的元信息（窗口内条数、缺口头、坏行、去重数）。
type EventMeta struct {
	Count      int `json:"count"`
	Files      int `json:"files"`
	Gaps       int `json:"gaps"`
	Unparsed   int `json:"unparsed"`
	Duplicates int `json:"duplicates"`
}

// LogSummary 是日志证据的精简视图。
//
// 它刻意不复用具体的日志解析包：报告引擎只消费"签名 + 样本"，由调用方（CLI）
// 把任意解析实现转成这个结构，于是引擎不绑定某一种日志格式。
type LogSummary struct {
	Files      int            `json:"files"`
	Unparsed   int            `json:"unparsed"`
	Truncated  bool           `json:"truncated"`
	Signatures []LogSignature `json:"signatures,omitempty"`
}

// LogSignature 一组同模板日志行的聚合。
type LogSignature struct {
	Pattern string      `json:"pattern"`
	Count   int         `json:"count"`
	First   time.Time   `json:"first"`
	Last    time.Time   `json:"last"`
	Samples []LogSample `json:"samples,omitempty"`
}

// LogSample 一条日志样本（原文在 File:Line 可回查）。
type LogSample struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Level  string `json:"level"`
	Logger string `json:"logger"`
	Msg    string `json:"msg"`
}

// Baseline 运行开始时的状态基线（由调用方采样；可空）。
type Baseline struct {
	CapturedAt time.Time      `json:"captured_at"`
	Counts     map[string]int `json:"counts,omitempty"`
}

// Input 是报告引擎给 Section/Pattern 的只读输入。
type Input struct {
	Run       RunMeta
	EventMeta EventMeta
	Events    []events.Event
	Logs      LogSummary
	DB        *gorm.DB
	Baseline  Baseline
	Degraded  []string
	Now       time.Time
	Mode      Mode
	Engine    EngineState
	// Inputs 是本次报告的自证信息（Generate 已填好快照时刻/窗口/事件覆盖，其余由调用方提供）。
	Inputs Inputs
}

// SectionReport 一节在报告里的落地形态。
type SectionReport struct {
	Key      string    `json:"key"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary,omitempty"`
	Metrics  any       `json:"metrics,omitempty"`
	Tables   []Table   `json:"tables,omitempty"`
	Checks   []Check   `json:"checks,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
}

// Report 一次运行的完整报告（report.json 的根）。
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// GeneratedAt 报告生成时刻（= 最近一次更新时间）：定时报告每次刷新都会更新它；
	// 同一天目录被定时/退出/命令反复覆盖时，以这里为准判断"这份报告是什么时候的"。
	GeneratedAt   time.Time         `json:"generated_at"`
	Verdict       Verdict           `json:"verdict"`
	Run           RunMeta           `json:"run"`
	EventMeta     EventMeta         `json:"event_meta"`
	Logs          LogSummary        `json:"logs,omitempty"`
	Environment   map[string]string `json:"environment,omitempty"`
	Degraded      []string          `json:"degraded,omitempty"`
	Sections      []SectionReport   `json:"sections"`
	Checks        []Check           `json:"checks"`
	Problems      []Problem         `json:"problems"`
	Mode          Mode              `json:"mode,omitempty"`
	VerdictReason string            `json:"verdict_reason,omitempty"`
	Engine        EngineState       `json:"engine"`
	// Inputs 是报告自证信息：这份报告基于哪一刻的快照、什么版本/配置/阈值、事件覆盖到哪。
	Inputs Inputs `json:"inputs,omitempty"`
}

// Options 生成报告所需的全部输入。
type Options struct {
	Run         RunMeta
	EventMeta   EventMeta
	Events      []events.Event
	Logs        LogSummary
	DB          *gorm.DB
	Baseline    Baseline
	Environment map[string]string
	Degraded    []string
	Sections    []Section
	Patterns    []Pattern
	Now         time.Time
	Mode        Mode
	Engine      EngineState
	// Inputs 由调用方填版本/commit/配置/阈值来源，Generate 负责补快照时刻、窗口与事件覆盖。
	Inputs Inputs
}

// Generate 依次跑 Section 与 Pattern，聚合成一份 Report。
//
// 单节失败不拖垮整份报告：记入 Degraded，其余节继续（对应"报告本身不能比被观测系统更脆"）。
func Generate(ctx context.Context, opts Options) (*Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// 自证信息以调用方提供的为底：快照时刻缺省=生成时刻（batch 1 语义），
	// 窗口与事件覆盖必须由本次运行推导，不接受调用方自相矛盾的填法。
	inp := opts.Inputs
	if inp.SnapshotAt.IsZero() {
		inp.SnapshotAt = now
	}
	inp.WindowFrom = opts.Run.From
	inp.WindowTo = opts.Run.To
	inp.Events = ComputeEventCoverage(opts.Events)
	in := &Input{
		Run:       opts.Run,
		EventMeta: opts.EventMeta,
		Events:    opts.Events,
		Logs:      opts.Logs,
		DB:        opts.DB,
		Baseline:  opts.Baseline,
		Now:       now,
		Mode:      opts.Mode,
		Engine:    opts.Engine,
		Inputs:    inp,
	}
	rep := &Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now,
		Run:           opts.Run,
		EventMeta:     opts.EventMeta,
		Logs:          opts.Logs,
		Environment:   opts.Environment,
		Degraded:      append([]string(nil), opts.Degraded...),
		Mode:          opts.Mode,
		Engine:        opts.Engine,
		Inputs:        inp,
	}
	for _, s := range opts.Sections {
		if s == nil {
			continue
		}
		res, err := s.Build(ctx, in)
		if err != nil {
			rep.Degraded = append(rep.Degraded, fmt.Sprintf("section %s: %v", s.Key(), err))
			continue
		}
		if res == nil {
			continue
		}
		rep.Sections = append(rep.Sections, SectionReport{
			Key: s.Key(), Title: s.Title(), Summary: res.Summary,
			Metrics: res.Metrics, Tables: res.Tables, Checks: res.Checks, Problems: res.Problems,
		})
		rep.Checks = append(rep.Checks, res.Checks...)
		rep.Problems = append(rep.Problems, res.Problems...)
	}
	for _, p := range opts.Patterns {
		if p == nil {
			continue
		}
		rep.Problems = append(rep.Problems, p.Match(in)...)
	}
	sortProblems(rep.Problems)
	// 分析器可能在 Input.Degraded 里追加（如 DB 未配置），一并带进报告。
	rep.Degraded = append(rep.Degraded, in.Degraded...)
	rep.Verdict = VerdictOf(rep.Checks)
	synthesizeProblems(rep)
	applyVerdictPolicy(rep)
	return rep, nil
}

// VerdictOf 聚合判定：任一 fail 级检查未通过 → fail；否则任一 warn 未通过 → warn；否则 pass。
// Skipped 的检查本次不适用，不参与判定。
func VerdictOf(checks []Check) Verdict {
	v := VerdictPass
	for _, c := range checks {
		if c.Passed || c.Skipped {
			continue
		}
		switch c.Severity {
		case SeverityFail:
			return VerdictFail
		case SeverityWarn:
			v = VerdictWarn
		}
	}
	return v
}

// applyVerdictPolicy 在聚合判定之后做 fail-closed 收口：
// 已经 fail 的不改；降级、无可判定检查或暂停快照下的 pass 一律降成 unknown。
func applyVerdictPolicy(rep *Report) {
	switch rep.Verdict {
	case VerdictFail:
		rep.VerdictReason = failReason(rep.Checks)
		return
	case VerdictWarn:
		rep.VerdictReason = warnReason(rep.Checks)
		return
	}
	if !hasApplicableChecks(rep.Checks) {
		rep.Verdict = VerdictUnknown
		rep.VerdictReason = "没有可判定的检查项（全部不适用或为空）"
		return
	}
	if len(rep.Degraded) > 0 {
		rep.Verdict = VerdictUnknown
		rep.VerdictReason = "报告降级，数据不完整：" + rep.Degraded[0]
		return
	}
	if rep.Mode == ModeSnapshot && rep.Engine.Inactive() {
		rep.Verdict = VerdictUnknown
		rep.VerdictReason = "引擎暂停/空闲，快照模式不评估运行健康"
	}
}

// hasApplicableChecks 只把 fail/warn 当判定项；info 只是提示，不承担「已检查」的语义。
func hasApplicableChecks(checks []Check) bool {
	for _, c := range checks {
		if c.Skipped || c.Severity == SeverityInfo {
			continue
		}
		return true
	}
	return false
}

func failedIDs(checks []Check, sev Severity) []string {
	out := make([]string, 0, 4)
	for _, c := range checks {
		if c.Skipped || c.Passed || c.Severity != sev {
			continue
		}
		out = append(out, c.ID)
	}
	return out
}

func failReason(checks []Check) string {
	ids := failedIDs(checks, SeverityFail)
	return fmt.Sprintf("%d 个适用 fail 检查未通过：%s", len(ids), strings.Join(ids, "、"))
}

func warnReason(checks []Check) string {
	ids := failedIDs(checks, SeverityWarn)
	return fmt.Sprintf("%d 个适用 warn 检查未通过：%s", len(ids), strings.Join(ids, "、"))
}

// synthesizeProblems 把未通过的 fail/warn 检查合成可行动 Problem（已有同 ID 的不重复）。
func synthesizeProblems(rep *Report) {
	have := make(map[string]bool, len(rep.Problems))
	for _, p := range rep.Problems {
		have[p.ID] = true
	}
	for _, c := range rep.Checks {
		if c.Passed || c.Skipped || c.Severity == SeverityInfo {
			continue
		}
		id := "check-" + c.ID
		if have[id] {
			continue
		}
		rep.Problems = append(rep.Problems, Problem{
			ID: id, Severity: c.Severity, Title: c.Message, Origin: "check",
			Evidence:   []Evidence{{Type: "check", Ref: c.ID, Excerpt: c.Detail}},
			Acceptance: "修复后重新生成报告，该检查通过",
		})
	}
	sortProblems(rep.Problems)
}

// NewCheck 构造一条判定项（少写重复字面量）。
func NewCheck(id string, sev Severity, passed bool, message, detail string) Check {
	return Check{ID: id, Severity: sev, Passed: passed, Message: message, Detail: detail}
}

// sortProblems 按 严重级 → 计数降序 → ID 排序，保证报告确定性。
func sortProblems(list []Problem) {
	rank := func(s Severity) int {
		switch s {
		case SeverityFail:
			return 0
		case SeverityWarn:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if rank(list[i].Severity) != rank(list[j].Severity) {
			return rank(list[i].Severity) < rank(list[j].Severity)
		}
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].ID < list[j].ID
	})
}
