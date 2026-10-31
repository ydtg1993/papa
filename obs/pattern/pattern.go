// Package pattern 是 papa 报告引擎的内置模式库：把通用证据（事件/日志）映射成
// 可行动问题（现象 → 证据 → 假设 → 验证 → 修复方向）。
//
// 只处理框架通用维度（运行边界/队列/事件丢失/熔断/阶段注册），不含任何业务语义；
// 业务模式由业务侧实现 obs.Pattern 注册进来。
package pattern

import (
	"fmt"
	"sort"
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

// ProcessRestartPattern 只提示缺少明确正常停止证据的重启。
type ProcessRestartPattern struct{}

func (p *ProcessRestartPattern) Key() string { return "process-restart" }

func (p *ProcessRestartPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil {
		return nil
	}
	var boundaries []events.Event
	for _, ev := range in.Events {
		if ev.Type == events.TypeRunStarted || ev.Type == events.TypeRunStopped {
			boundaries = append(boundaries, ev)
		}
	}
	sort.SliceStable(boundaries, func(i, j int) bool { return boundaries[i].TS.Before(boundaries[j].TS) })
	var previous *events.Event
	var stopped *events.Event
	var uncertain int
	var evidence []obs.Evidence
	for i := range boundaries {
		ev := &boundaries[i]
		if ev.Type == events.TypeRunStopped {
			if previous != nil && ev.RunID == previous.RunID {
				stopped = ev
			}
			continue
		}
		if previous != nil {
			// 同一 run_id 的重复开始记录不能成为重启证据。
			if ev.RunID != "" && ev.RunID == previous.RunID {
				continue
			}
			if !normalStop(stopped) {
				uncertain++
				excerpt := "未找到匹配的 run_stopped，退出原因未知"
				if stopped != nil {
					excerpt = fmt.Sprintf("run_stopped outcome=%q unfinished=%d queued=%d error=%q",
						stopped.Outcome, stopped.Counts["unfinished"], stopped.Counts["queued"], stopped.Error)
				}
				evidence = append(evidence, obs.Evidence{Type: "event",
					Ref: fmt.Sprintf("run_id=%s → run_id=%s", previous.RunID, ev.RunID), Excerpt: excerpt})
			}
		}
		previous, stopped = ev, nil
	}
	if uncertain == 0 {
		return nil
	}
	return []obs.Problem{{
		ID: "process-restarts", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("窗口内 %d 次重启缺少明确正常的停止证据", uncertain),
		Count: uncertain, Evidence: evidence,
		Hypotheses: []obs.Hypothesis{{
			Text: "结束事件缺失或退出记录有错误，当前证据不足以确定退出原因", Confidence: "low",
			Verify:  "对照匹配 run_id 的 run_stopped.outcome、部署记录与系统日志；启动次数不能证明崩溃或 OOM",
			FixHint: "确认退出与事件记录是否完整；未完成任务由 recover_queue 在下次启动恢复",
		}},
		Acceptance: "计划重启有匹配的正常 run_stopped；异常退出有独立证据说明原因",
	}}
}

func normalStop(ev *events.Event) bool {
	// partial 表示有明确的停止收尾，未完成任务留待下次恢复；存留数量本身不是异常退出证据。
	return ev != nil && ev.Error == "" && (ev.Outcome == "drained" || ev.Outcome == "partial")
}

// QueueNotDrainingPattern：最后一次快照队列仍有积压，且窗口内没有轮次处理。
type QueueNotDrainingPattern struct{}

func (p *QueueNotDrainingPattern) Key() string { return "queue-not-draining" }

func (p *QueueNotDrainingPattern) Match(in *obs.Input) []obs.Problem {
	if in == nil || (in.Mode == obs.ModeSnapshot && in.Engine.Inactive()) {
		return nil
	}
	var last *events.Event
	var rounds int
	activeSites := map[string]bool{}
	pausedSites := map[string]bool{}
	if in.Mode == obs.ModeSnapshot {
		for _, site := range in.Engine.Sites {
			pausedSites[site.Key] = site.Paused
		}
	}
	for i := range in.Events {
		ev := &in.Events[i]
		if ev.Type == events.TypeStageSnapshot && (last == nil || !ev.TS.Before(last.TS)) {
			last = ev // 空 stages 也代表有效的末次状态，不能使用更早的积压。
		}
		if ev.Type == events.TypeQueueRound {
			rounds++
		}
		switch ev.Type {
		case events.TypeTaskClaimed, events.TypeTaskFinished, events.TypeTaskFailed, events.TypeTaskRetry:
			if !pausedSites[ev.Site] {
				activeSites[ev.Site] = true
			}
		}
	}
	if last == nil || len(activeSites) == 0 {
		return nil
	}
	var backlog int
	for key, s := range last.Stages {
		site := s.Site
		if site == "" {
			site, _, _ = strings.Cut(key, ":")
		}
		if pausedSites[site] || (!activeSites[site] && !activeSites[""]) {
			continue
		}
		backlog += s.InProgress // 已含 QueueLen，不能把排队再累加一遍。
	}
	for _, q := range last.Queues {
		if q.Enabled || q.Running {
			backlog += int(q.Backlog)
		}
	}
	if backlog <= 0 || rounds > 0 {
		return nil
	}
	return []obs.Problem{{
		ID: "queue-not-draining", Severity: obs.SeverityWarn,
		Title: fmt.Sprintf("有执行活动的队列末次快照仍有 %d 条未完成/积压，且窗口内没有队列轮次", backlog),
		Count: backlog,
		Hypotheses: []obs.Hypothesis{{
			Text: "执行窗口仍有未完成任务，需结合负载判断队列是否在收敛", Confidence: "low",
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
