// Package pattern 是 papa 报告引擎的内置模式库：把通用证据（事件/日志）映射成
// 可行动问题（现象 → 证据 → 假设 → 验证 → 修复方向）。
//
// 只处理框架通用维度（运行边界/队列/事件丢失/熔断/阶段注册），不含任何业务语义；
// 业务模式由业务侧实现 obs.Pattern 注册进来。
package pattern

import (
	"fmt"
	"strings"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// Builtin 返回框架内置模式（确定性顺序）。
func Builtin() []obs.Pattern {
	return []obs.Pattern{
		&EventLossPattern{},
		&ProcessRestartPattern{},
		&QueueNotDrainingPattern{},
		&BreakerNoisePattern{},
		&StageNotRegisteredPattern{},
	}
}

// EventLossPattern：seq 缺口 = 丢弃（buffer 满/写失败）或轮转丢行。
type EventLossPattern struct{}

func (p *EventLossPattern) Key() string { return "event-loss" }

func (p *EventLossPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil || in.EventMeta.Gaps <= 0 {
		return nil
	}
	return []obs.Problem{{
		ID: "event-loss", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("事件流有 %d 个 seq 缺口（有事件被丢弃或丢失）", in.EventMeta.Gaps),
		Count: in.EventMeta.Gaps,
		Evidence: []obs.Evidence{{
			Type: "event", Ref: "event_meta.gaps",
			Excerpt: fmt.Sprintf("files=%d unparsed=%d duplicates=%d", in.EventMeta.Files, in.EventMeta.Unparsed, in.EventMeta.Duplicates),
		}},
		Hypotheses: []obs.Hypothesis{{
			Text: "事件写入跟不上（buffer 满被丢弃），或轮转/磁盘问题丢行", Confidence: "high",
			Verify:  "查 sys.log 里是否有 event stream dropped；对比 run_stopped 的 dropped_events",
			FixHint: "调大 obs.events.buffer 或降低事件密度（snapshot_interval）",
		}},
		Acceptance: "重跑后 events.jsonl 的 seq 连续（缺口为 0）",
	}}
}

// ProcessRestartPattern：窗口内多次 run_started = 进程重启过。
type ProcessRestartPattern struct{}

func (p *ProcessRestartPattern) Key() string { return "process-restart" }

func (p *ProcessRestartPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil {
		return nil
	}
	var starts int
	for _, ev := range in.Events {
		if ev.Type == events.TypeRunStarted {
			starts++
		}
	}
	if starts <= 1 {
		return nil
	}
	return []obs.Problem{{
		ID: "process-restarts", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("窗口内进程启动 %d 次（发生过重启）", starts),
		Count: starts,
		Hypotheses: []obs.Hypothesis{{
			Text: "崩溃/被 OOM/被部署重启，而不是计划内重启", Confidence: "medium",
			Verify:  "对照 run_stopped.outcome 与系统日志；看是否有未排空任务",
			FixHint: "定位重启原因；重启后由 recover_queue 兜底重投",
		}},
		Acceptance: "重跑窗口内只有 1 次 run_started",
	}}
}

// QueueNotDrainingPattern：最后一次快照队列仍有积压，且窗口内没有轮次处理。
type QueueNotDrainingPattern struct{}

func (p *QueueNotDrainingPattern) Key() string { return "queue-not-draining" }

func (p *QueueNotDrainingPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil {
		return nil
	}
	var last *events.Event
	var rounds int
	for i := range in.Events {
		ev := &in.Events[i]
		if ev.Type == events.TypeStageSnapshot && len(ev.Stages) > 0 {
			last = ev
		}
		if ev.Type == events.TypeQueueRound {
			rounds++
		}
	}
	if last == nil {
		return nil
	}
	var backlog int
	for _, s := range last.Stages {
		backlog += s.QueueLen + s.InProgress
	}
	for _, q := range last.Queues {
		backlog += int(q.Backlog)
	}
	if backlog <= 0 || rounds > 0 {
		return nil
	}
	return []obs.Problem{{
		ID: "queue-not-draining", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("队列末次快照仍有 %d 条积压/在跑，且窗口内没有队列轮次", backlog),
		Count: backlog,
		Hypotheses: []obs.Hypothesis{{
			Text: "worker 被暂停/熔断闸住，或队列治理未在运行", Confidence: "medium",
			Verify:  "查 /api/monitor 的 queues/stages 与熔断状态；确认队列 enabled",
			FixHint: "检查站点暂停/熔断、worker pool 回收、队列配置",
		}},
		Acceptance: "重跑后队列收敛（期末积压为 0 或按负载解释）",
	}}
}

// BreakerNoisePattern：熔断触发次数偏多。
type BreakerNoisePattern struct{}

func (p *BreakerNoisePattern) Key() string { return "breaker-noise" }

func (p *BreakerNoisePattern) Match(in *obs.Input) []obs.Problem {
	if in == nil {
		return nil
	}
	counts := map[string]int{}
	for _, ev := range in.Events {
		if ev.Type != events.TypeBreakerTripped {
			continue
		}
		counts[ev.Site]++
	}
	var total int
	for _, n := range counts {
		total += n
	}
	if total < 2 {
		return nil
	}
	return []obs.Problem{{
		ID: "breaker-noise", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("窗口内熔断触发 %d 次（可能阈值过低或坏 URL 集中）", total),
		Count: total,
		Hypotheses: []obs.Hypothesis{{
			Text: "坏 URL/页面结构错误集中触发熔断，阈值偏低掩盖了真实失败率", Confidence: "medium",
			Verify:  "对照失败分组：熔断窗口内的失败是否集中在少数 URL",
			FixHint: "crawler.breaker.threshold/window 或先修失败源",
		}},
		Acceptance: "修复失败源后熔断触发次数下降，且不误伤正常抓取",
	}}
}

// StageNotRegisteredPattern：日志里出现"阶段未注册"（历史任务与当前声明不一致）。
type StageNotRegisteredPattern struct{}

func (p *StageNotRegisteredPattern) Key() string { return "stage-not-registered" }

func (p *StageNotRegisteredPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil {
		return nil
	}
	for _, sig := range in.Logs.Signatures {
		if !strings.Contains(sig.Pattern, "not registered") {
			continue
		}
		ev := []obs.Evidence{{
			Type: "log", Ref: fmt.Sprintf("%s", sig.Pattern),
			Excerpt: fmt.Sprintf("count=%d", sig.Count),
		}}
		if len(sig.Samples) > 0 {
			s := sig.Samples[0]
			ev[0].Ref = fmt.Sprintf("%s:%d", s.File, s.Line)
			ev[0].Excerpt = s.Msg
		}
		return []obs.Problem{{
			ID: "stage-not-registered", Severity: obs.SeverityWarn,
			Title: fmt.Sprintf("历史任务引用了未注册的阶段（日志命中 %d 次）", sig.Count),
			Count: sig.Count, Evidence: ev,
			Hypotheses: []obs.Hypothesis{{
				Text: "站点/阶段声明改过名或删过阶段，旧任务留在库里", Confidence: "high",
				Verify:  "对照 crawler_tasks 的 site/stage 与 sites/*.go 声明",
				FixHint: "补回阶段声明，或清理/重投这些历史任务",
			}},
			Acceptance: "重跑后日志不再出现 not registered",
		}}
	}
	return nil
}
