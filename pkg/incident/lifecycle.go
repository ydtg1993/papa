package incident

import (
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/redact"
)

const MaxTransitions = 128
const MaxFailureGroups = 64
const MaxStageChecks = 128

// StageCheck is this recovery round's evidence, not a second failure counter.
type StageCheck struct {
	Stage            string    `json:"stage"`
	State            string    `json:"state"`
	Round            int       `json:"round"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	Detail           string    `json:"detail"`
	TaskSuccesses    int       `json:"task_successes"`
	Verification     string    `json:"verification"`
	Coverage         string    `json:"coverage"`
	ProbeSuccesses   int       `json:"probe_successes"`
	Verified         bool      `json:"verified"`
	VerificationNote string    `json:"verification_note"`
	RequiredTasks    int       `json:"required_tasks"`
	RequiredProbes   int       `json:"required_probes"`
	SampleIDs        []string  `json:"sample_ids,omitempty"`
	TimedOutIDs      []string  `json:"timed_out_ids,omitempty"`
}

// FailureGroup records structured classification, independent of text samples.
type FailureGroup struct {
	Stage       string `json:"stage"`
	Category    string `json:"category"`
	Fingerprint string `json:"fingerprint"`
	Count       int    `json:"count"`
	Tasks       int    `json:"tasks"`
}

// Lifecycle is the durable, bounded report of one automatic protection event.
// Capture describes the triggering window. Evidence and Timeline survive restart.
type Lifecycle struct {
	Capture         Capture        `json:"capture"`
	Stage           string         `json:"stage"`
	State           string         `json:"state"`
	Revision        uint64         `json:"revision"`
	Rule            string         `json:"rule"`
	Category        string         `json:"category"`
	Fingerprint     string         `json:"fingerprint"`
	Epoch           uint64         `json:"epoch"`
	StartedAt       time.Time      `json:"started_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	LastFailureAt   time.Time      `json:"last_failure_at"`
	NextProbeAt     time.Time      `json:"next_probe_at"`
	Deadline        time.Time      `json:"deadline"`
	FinalAt         time.Time      `json:"final_at"`
	ProbeAttempts   int            `json:"probe_attempts"`
	Checks          []StageCheck   `json:"checks"`
	ChecksLimited   bool           `json:"checks_limited"`
	CountersResetAt time.Time      `json:"counters_reset_at"`
	Successes       int            `json:"successes"`
	WindowFailures  int            `json:"window_failures"`
	WindowSuccesses int            `json:"window_successes"`
	ShortFailures   int            `json:"short_failures"`
	DistinctTasks   int            `json:"distinct_tasks"`
	Groups          []FailureGroup `json:"groups"`
	AutoThrottle    bool           `json:"autothrottle"`
	RetryAfterUntil time.Time      `json:"retry_after_until"`
	Evidence        []Sample       `json:"evidence"`
	Timeline        []Transition   `json:"timeline"`
	Hypotheses      []string       `json:"hypotheses"`
	Contradictions  []string       `json:"contradictions"`
	Missing         []string       `json:"missing"`
	Conclusion      string         `json:"conclusion"`
	Limited         bool           `json:"limited"`
	RecheckAt       time.Time      `json:"recheck_at"`
	Cycle           int            `json:"cycle"`
}

type Transition struct {
	Stage         string      `json:"stage,omitempty"`
	Round         int         `json:"round"`
	Failures      int         `json:"failures"`
	Successes     int         `json:"successes"`
	NextAttemptAt time.Time   `json:"next_attempt_at"`
	At            time.Time   `json:"at"`
	From          string      `json:"from"`
	To            string      `json:"to"`
	Action        string      `json:"action"`
	Reason        string      `json:"reason"`
	ProbeOK       *bool       `json:"probe_ok,omitempty"`
	Sequence      uint64      `json:"sequence,omitempty"`
	Cycle         int         `json:"cycle"`
	Check         *StageCheck `json:"check,omitempty"`
}

// Active includes manual-attention events: they still own an automatic pause.
func (l Lifecycle) Active() bool { return l.State != "recovered" && l.State != "closed" }

func safeDetail(v string, n int) string {
	if len(v) > 16384 {
		return "[内容超过采集上限]"
	}
	return clip(redact.Text(v), n)
}

func cleanDetails(in []string) []string {
	out := make([]string, 0, min(len(in), 32))
	for _, v := range in[:min(len(in), 32)] {
		out = append(out, safeDetail(v, 1024))
	}
	return out
}

// Clean returns detached data and never mutates the engine's live state.
func (l Lifecycle) Clean() Lifecycle {
	l.Capture.Samples = nil
	l.Capture.Site = safeDetail(l.Capture.Site, 512)
	l.Capture.ID = safeDetail(l.Capture.ID, 128)
	l.Stage = safeDetail(l.Stage, 512)
	l.State = safeDetail(l.State, 32)
	l.Rule = safeDetail(l.Rule, 64)
	l.Category = safeDetail(l.Category, 128)
	l.Fingerprint = safeDetail(l.Fingerprint, 256)
	l.Conclusion = safeDetail(l.Conclusion, 2048)
	l.ChecksLimited = l.ChecksLimited || len(l.Checks) > MaxStageChecks
	checks := make([]StageCheck, 0, min(len(l.Checks), MaxStageChecks))
	for _, check := range l.Checks[:min(len(l.Checks), MaxStageChecks)] {
		check.Stage, check.State = safeDetail(check.Stage, 512), safeDetail(check.State, 32)
		check.Detail = safeDetail(check.Detail, 1024)
		check.Coverage = safeDetail(check.Coverage, 1024)
		check.SampleIDs = append([]string(nil), check.SampleIDs[:min(3, len(check.SampleIDs))]...)
		for i := range check.SampleIDs {
			check.SampleIDs[i] = safeDetail(check.SampleIDs[i], 1024)
		}
		check.TimedOutIDs = append([]string(nil), check.TimedOutIDs[:min(3, len(check.TimedOutIDs))]...)
		for i := range check.TimedOutIDs {
			check.TimedOutIDs[i] = safeDetail(check.TimedOutIDs[i], 1024)
		}
		if check.Verification == "readonly" {
			check.RequiredProbes = max(2, check.RequiredProbes)
			check.VerificationNote = fmt.Sprintf("未验证：完整只读检查 %d/%d 次", check.ProbeSuccesses, check.RequiredProbes)
			if check.Verified {
				check.VerificationNote = "完整只读验证通过；不冒充真实任务执行成功"
			}
		} else {
			check.Verification = "task"
			if check.Coverage == "" {
				check.Coverage = "完整真实任务路径；未声明只读检查可完整覆盖"
			}
			minimum := max(1, check.RequiredTasks)
			check.VerificationNote = fmt.Sprintf("未验证：仍需本轮真实任务成功（%d/%d）", check.TaskSuccesses, minimum)
			if check.Verified {
				check.VerificationNote = "本轮真实任务验证通过"
			}
		}
		if check.State == "failed" || check.State == "missing" || check.State == "interrupted" {
			check.Verified = false
			check.VerificationNote = "未验证：" + check.Detail
		}
		if len(check.TimedOutIDs) > 0 {
			check.Verified = false
			check.VerificationNote = fmt.Sprintf("未验证：本周期样本 %v 超时；其它样本通过不能代替，下周期重新验证", check.TimedOutIDs)
		}
		checks = append(checks, check)
	}
	l.Checks = checks
	l.Limited = l.Limited || len(l.Evidence) > MaxSamples || len(l.Timeline) > MaxTransitions || len(l.Groups) > MaxFailureGroups || len(l.Hypotheses) > 32 || len(l.Contradictions) > 32 || len(l.Missing) > 32
	groups := make([]FailureGroup, 0, min(len(l.Groups), MaxFailureGroups))
	for _, group := range l.Groups[:min(len(l.Groups), MaxFailureGroups)] {
		group.Stage = safeDetail(group.Stage, 512)
		group.Category = safeDetail(group.Category, 128)
		group.Fingerprint = safeDetail(group.Fingerprint, 256)
		groups = append(groups, group)
	}
	l.Groups = groups
	evidence := make([]Sample, 0, min(len(l.Evidence), MaxSamples))
	for _, sample := range l.Evidence[:min(len(l.Evidence), MaxSamples)] {
		evidence = append(evidence, Clean(sample))
	}
	l.Evidence = evidence
	// Retain the initial transition and the newest tail when bounded history fills.
	history := l.Timeline
	if len(history) > MaxTransitions {
		history = append([]Transition{history[0]}, history[len(history)-(MaxTransitions-1):]...)
	}
	l.Timeline = make([]Transition, len(history))
	for i, entry := range history {
		entry.Stage = safeDetail(entry.Stage, 512)
		entry.From, entry.To = safeDetail(entry.From, 32), safeDetail(entry.To, 32)
		entry.Action, entry.Reason = safeDetail(entry.Action, 128), safeDetail(entry.Reason, 1024)
		if entry.Check != nil {
			cleaned := (Lifecycle{Checks: []StageCheck{*entry.Check}}).Clean()
			entry.Check = &cleaned.Checks[0]
		}
		if entry.ProbeOK != nil {
			value := *entry.ProbeOK
			entry.ProbeOK = &value
		}
		l.Timeline[i] = entry
	}
	l.Hypotheses, l.Contradictions, l.Missing = cleanDetails(l.Hypotheses), cleanDetails(l.Contradictions), cleanDetails(l.Missing)
	return l
}

func (l Lifecycle) Analysis() Report {
	l = l.Clean()
	c := l.Capture
	c.Samples = l.Evidence
	c.Limited = c.Limited || l.Limited
	r := Analyze(c)
	r.Hypotheses, r.Contradictions, r.Missing = cleanDetails(l.Hypotheses), cleanDetails(l.Contradictions), cleanDetails(l.Missing)
	add := func(dst *[]string, v string) {
		for _, existing := range *dst {
			if existing == v {
				return
			}
		}
		if len(*dst) < 32 {
			*dst = append(*dst, v)
		}
	}
	categories := map[string]bool{}
	for _, g := range l.Groups {
		if g.Count > 0 {
			categories[g.Category] = true
		}
	}
	if len(categories) == 0 && l.Category != "" {
		categories[l.Category] = true
	}
	// Iterate in a fixed order so equal revisions serialize identically.
	for _, category := range []string{"rate_limit", "not_found", "structure", "authentication", "access", "dns", "tls", "timeout", "connection", "server", "local", "http", "unknown"} {
		if !categories[category] {
			continue
		}
		switch category {
		case "rate_limit":
			add(&r.Hypotheses, "已观察到限流信号；访问频率、账号配额或共享出口限制均需核实。")
			add(&r.Missing, "需对照 Retry-After、实际发起频率及降频后的同类页面结果，验证限流是否缓解。")
			if !l.AutoThrottle {
				add(&r.Missing, "AutoThrottle 未开启，不能把暂停后的恢复归功于自动降频。")
			}
		case "not_found":
			add(&r.Hypotheses, "部分资源可能删除、迁移，或 URL 生成规则失效；404/410 不能单独证明整站宕机。")
			add(&r.Missing, "需用当前有效页面与失败页面对照，检查重定向、资源目录和 URL 规则。")
		case "structure":
			add(&r.Hypotheses, "页面结构、加载条件或语义校验可能变化；正常 HTTP 响应不代表采集已恢复。")
			add(&r.Missing, "需对失败页面执行原阶段的节点提取与语义校验，确认内容是否满足业务契约。")
		case "authentication", "access":
			add(&r.Hypotheses, "认证、授权或访问验证可能阻断当前任务；不能仅凭拒绝访问认定频率过高。")
			add(&r.Missing, "需核实账号状态、会话有效性和访问验证页面，并验证原阶段语义。")
		case "dns", "tls", "timeout", "connection":
			add(&r.Hypotheses, "链路、代理、DNS、TLS 或服务端响应可能异常；当前证据不能确定故障归属。")
			add(&r.Missing, "需比较相同出口的成功请求、代理健康及低并发试探，区分本地链路与目标故障。")
		case "server":
			add(&r.Hypotheses, "服务端或中间网关返回错误；需核实影响范围，不能直接认定整个网站宕机。")
			add(&r.Missing, "需对照多个有效页面、响应来源和低频试探结果，确认错误是否持续。")
		case "local":
			add(&r.Hypotheses, "本地浏览器、资源或存储故障可能导致任务失败；应先检查本地运行环境。")
			add(&r.Contradictions, "本地基础设施错误不构成目标网站宕机的证据。")
		default:
			add(&r.Missing, "当前分类不足以归因；需补充结构化错误码、失败阶段及可对照的成功样本。")
		}
	}
	if len(categories) > 1 {
		add(&r.Missing, "多类错误同时出现，需分别验证限流、页面迁移与网络问题；混合错误本身不证明频率过高。")
	}
	if l.WindowSuccesses > 0 {
		add(&r.Contradictions, "同一统计范围内仍有成功样本，不支持所有页面均不可用的结论；成功不抵消失败计数。")
	}
	if l.DistinctTasks > 0 && l.DistinctTasks < 2 {
		add(&r.Missing, "失败仅涉及一个已知任务，不能据此推断其他页面的可用性。")
	}
	if l.Limited {
		add(&r.Missing, "证据已截断，未保存的页面或错误类型无法排除。")
	}
	return r
}

func (l Lifecycle) Text() string {
	l = l.Clean()
	var b strings.Builder
	fmt.Fprintf(&b, "故障事件 %s\n状态：%s；修订：%d；规则：%s\n范围：%s / %s\n开始：%s；更新：%s\n成功样本：%d；试探次数：%d\nAutoThrottle 开启：%t\n", l.Capture.ID, l.State, l.Revision, l.Rule, l.Capture.Site, l.Stage, l.StartedAt.Format(time.RFC3339), l.UpdatedAt.Format(time.RFC3339), l.Successes, l.ProbeAttempts, l.AutoThrottle)
	fmt.Fprintf(&b, "触发瞬间失败：%d；当前窗口失败：%d；当前窗口成功：%d；短窗口失败：%d；已知失败任务：%d\n", l.Capture.Failures, l.WindowFailures, l.WindowSuccesses, l.ShortFailures, l.DistinctTasks)
	if !l.NextProbeAt.IsZero() {
		fmt.Fprintf(&b, "下次试探：%s\n", l.NextProbeAt.Format(time.RFC3339))
	}
	if !l.RecheckAt.IsZero() {
		fmt.Fprintf(&b, "下一轮小时复查：%s；恢复周期：%d\n", l.RecheckAt.Format(time.RFC3339), l.Cycle)
	}
	if !l.RetryAfterUntil.IsZero() {
		fmt.Fprintf(&b, "服务端最早请求时间：%s\n", l.RetryAfterUntil.Format(time.RFC3339))
	}
	if !l.FinalAt.IsZero() {
		fmt.Fprintf(&b, "结案：%s\n", l.FinalAt.Format(time.RFC3339))
	}
	if !l.CountersResetAt.IsZero() {
		fmt.Fprintf(&b, "整站验证完成，运行计数清零：%s；以下统计为清零前保留的证据\n", l.CountersResetAt.Format(time.RFC3339))
	}
	b.WriteString("\n全阶段验证进度\n")
	for _, check := range l.Checks {
		fmt.Fprintf(&b, "%s：%s；方式=%s；覆盖范围=%s；%s；第 %d 轮；探针成功 %d 次；本轮真实任务成功 %d 次；%s\n", check.Stage, check.State, check.Verification, check.Coverage, check.VerificationNote, check.Round, check.ProbeSuccesses, check.TaskSuccesses, check.Detail)
	}
	if l.ChecksLimited {
		b.WriteString("阶段清单超出上限，不能认定整站恢复。\n")
	}
	fmt.Fprintf(&b, "结论：%s\n", l.Conclusion)
	analysis := l.Analysis()
	for _, section := range []struct {
		name   string
		values []string
	}{{"支持的假设", analysis.Hypotheses}, {"反证", analysis.Contradictions}, {"缺失信息", analysis.Missing}} {
		for _, v := range section.values {
			fmt.Fprintf(&b, "%s：%s\n", section.name, v)
		}
	}
	b.WriteString("\n当前窗口结构化错误分组\n")
	for _, g := range l.Groups {
		fmt.Fprintf(&b, "%s / %s：%d 次，%d 个任务；指纹：%s\n", g.Stage, g.Category, g.Count, g.Tasks, g.Fingerprint)
	}
	if l.Limited {
		b.WriteString("说明：事件证据或时间线达到上限，不能推断未保存部分。\n")
	}
	b.WriteString("\n事件时间线\n")
	for _, item := range l.Timeline {
		fmt.Fprintf(&b, "%s %s → %s：%s；阶段 %s；第 %d 轮；窗口失败/成功 %d/%d；%s\n", item.At.Format(time.RFC3339), item.From, item.To, item.Action, item.Stage, item.Round, item.Failures, item.Successes, item.Reason)
		if !item.NextAttemptAt.IsZero() {
			fmt.Fprintf(&b, "  下次计划：%s\n", item.NextAttemptAt.Format(time.RFC3339))
		}
		if item.Check != nil {
			fmt.Fprintf(&b, "  当时验证方式=%s；覆盖=%s；样本=%v；%s\n", item.Check.Verification, item.Check.Coverage, item.Check.SampleIDs, item.Check.VerificationNote)
		}
	}
	b.WriteString("\n触发时的失败证据\n")
	b.WriteString(Text(l.Analysis()))
	return b.String()
}
