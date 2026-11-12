package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/obs/incident"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

// The registry defines coverage, including stages that have not run yet. The
// evidence fallback supports direct Engine users without a stage registry.
func (e *Engine) resetBreakerChecksLocked(s *smartBreaker) {
	previous := make(map[string][]breaker.RecoverySample)
	for _, c := range s.event.Checks {
		previous[c.Stage] = c.Samples
	}
	names := map[string]bool{}
	for _, stage := range e.stages {
		if stage.config.Site == s.event.Site {
			names[stage.stage] = true
		}
	}
	if len(names) == 0 {
		for _, sample := range s.detector.Checkpoint().Failures {
			names[sample.Stage] = true
		}
	}
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	s.event.ChecksLimited = len(keys) > breaker.MaxRecoveryStages
	s.event.Checks = nil
	for _, stage := range keys[:min(len(keys), breaker.MaxRecoveryStages)] {
		check := breaker.StageCheck{Stage: stage, State: "pending", Round: s.event.ProbeAttempts, Verification: "task", Coverage: "完整真实任务路径；未声明只读检查可完整覆盖", RequiredTasks: 1, RequiredProbes: 1}
		if info := e.stages[core.StageKey(s.event.Site, stage)]; info != nil && strings.TrimSpace(info.config.BreakerReadOnlyCoverage) != "" {
			check.Verification, check.Coverage = "readonly", info.config.BreakerReadOnlyCoverage
			check.RequiredProbes = 2
		}
		if s.event.MixedSamples {
			check.RequiredTasks, check.RequiredProbes = 3, 3
			seen := map[string]bool{}
			add := func(sample breaker.RecoverySample) {
				key := recoverySampleID(sample)
				if key != "" && !seen[key] && len(check.Samples) < 3 {
					seen[key] = true
					sample.Successes = 0
					check.Samples = append(check.Samples, sample)
				}
			}
			// Keep the selected fault identities across restart and natural window expiry.
			for _, sample := range previous[stage] {
				add(sample)
			}
			for _, failure := range s.detector.Checkpoint().Failures {
				if failure.Stage == stage {
					add(breaker.RecoverySample{TaskID: failure.TaskID, URL: failure.URL})
				}
			}
		}
		s.event.Checks = append(s.event.Checks, check)
	}
}

// One bounded timeline is shared by persistence, the monitor and obs. Evidence
// progress does not reset the observation timer or the control-save barrier.
func (e *Engine) noteBreakerLocked(s *smartBreaker, action, stage, reason string, now time.Time) {
	l := s.event
	e.appendBreakerTransitionLocked(s, incident.Transition{At: now, From: l.State, To: l.State, Action: action, Stage: stage, Round: l.ProbeAttempts, Failures: l.report.WindowFailures, Successes: l.report.WindowSuccesses, NextAttemptAt: l.NextProbeAt, Reason: reason})
	l.Revision++
	l.trimReport()
	s.dirty = true
	e.emit(events.Event{Type: events.TypeBreakerTransition, Site: l.Site, Stage: stage, Attrs: map[string]string{"incident_id": l.ID, "state": l.State, "action": action, "round": fmt.Sprint(l.ProbeAttempts)}})
}

func (e *Engine) appendBreakerTransitionLocked(s *smartBreaker, entry incident.Transition) {
	l := s.event
	if l.State == "manual" {
		entry.NextAttemptAt = l.RecheckAt
	}
	// A write outage cannot create unbounded memory or silently certify recovery.
	// This helper can be called while pauseMu is held by an operator action.
	if len(l.pendingHistory) >= 4096 {
		l.report.Limited = true
		if len(l.pendingHistory) > 4096 {
			return
		}
		if l.Active() {
			l.State = "manual"
			l.FinalAt = time.Time{}
			l.RecheckAt = entry.At.Add(time.Hour)
			l.NextProbeAt = l.RecheckAt
			s.controlRevision = l.Revision + 1 // admission stays blocked until durable; no nested pause lock
		}
		entry = incident.Transition{At: entry.At, To: l.State, Action: "history_capacity", Reason: "未保存历史达到4096条；后续过程可能不完整，停止自动恢复并等待存储恢复", NextAttemptAt: l.RecheckAt}
	}
	l.HistorySequence++
	entry.Sequence, entry.Cycle = l.HistorySequence, l.Cycle
	if entry.Stage != "" {
		for _, check := range l.reportSnapshot().Checks {
			if check.Stage == entry.Stage {
				copy := check
				entry.Check = &copy
				break
			}
		}
	}
	l.pendingHistory = append(l.pendingHistory, entry)
	l.report.Timeline = append(l.report.Timeline, entry)
}

func (e *Engine) recordBreakerStageSuccessLocked(s *smartBreaker, task *Task, now time.Time) {
	if task.breakerTrial != s.trial {
		return
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.Stage != task.Stage {
			continue
		}
		if s.event.MixedSamples && len(check.Samples) > 0 {
			matched := false
			for j := range check.Samples {
				sample := &check.Samples[j]
				if recoverySampleID(*sample) != recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL}) {
					continue
				}
				need := 3 / len(check.Samples)
				if j < 3%len(check.Samples) {
					need++
				}
				if sample.Successes < need {
					sample.Successes++
					matched = true
				}
			}
			if !matched {
				return
			}
		}
		if s.event.MixedSamples && len(check.Samples) == 0 {
			id := recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL})
			if id == "" {
				return
			}
			for _, prior := range check.SuccessIDs {
				if prior == id {
					return
				}
			}
			if len(check.SuccessIDs) >= 3 {
				return
			}
			check.SuccessIDs = append(check.SuccessIDs, id)
		}
		check.TaskSuccesses++
		if s.event.State == "observing" {
			check.State = "observing"
		}
		// First successes and the three-sample proof are milestones; avoid
		// writing one timeline entry for every successful page.
		if check.TaskSuccesses <= 3 {
			e.noteBreakerLocked(s, "stage_task_success", task.Stage, fmt.Sprintf("本轮受控真实任务 %d / 执行轮次 %s 完成；阶段成功不单独解除整站保护", task.ID, task.breakerRound), now)
		}
		return
	}
}

func (e *Engine) breakerStagesReadyLocked(s *smartBreaker, minimum int) bool {
	if s.event.ChecksLimited || len(s.event.Checks) == 0 {
		return false
	}
	for _, check := range s.event.Checks {
		if check.Verification == "readonly" {
			if check.State != "passed" || check.ProbeSuccesses < check.RequiredProbes {
				return false
			}
			continue
		}
		if check.TaskSuccesses < max(minimum, check.RequiredTasks) {
			return false
		}
	}
	return true
}

func (e *Engine) finishBreakerRecoveryLocked(s *smartBreaker, now time.Time) {
	if len(s.event.pendingHistory) >= 4095 {
		e.transitionBreakerLocked(s, "manual", "历史写入积压，不能完成恢复或清零", now)
		e.pauseSmartBreakerLocked(s, "历史写入积压")
		return
	}
	if !s.draining {
		s.draining = true
		e.noteBreakerLocked(s, "waiting_inflight", "", fmt.Sprintf("所有阶段验证通过，停止新增放行，等待 %d 个在途任务结束", s.inflight), now)
	}
	if s.inflight != 0 {
		return
	}
	// Freeze pre-reset totals in the report, then start the new site window.
	e.updateBreakerEvidenceLocked(s, s.detector.Snapshot(now))
	s.epoch++
	s.detector.ResetEpoch(s.epoch)
	s.event.CountersResetAt = now
	e.transitionBreakerLocked(s, "recovered", "所有阶段验证通过，全站在途任务已结束；清空运行计数，保留故障历史，报告保存后恢复正常运行", now)
}

// Prepare and persist each stage's intent before invoking extension code.
func (e *Engine) prepareNextBreakerProbeLocked(s *smartBreaker, now time.Time) bool {
	if s.event.ChecksLimited || len(s.event.Checks) == 0 {
		e.transitionBreakerLocked(s, "manual", "阶段验证清单为空或超出上限，不能确认整站恢复", now)
		return false
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State == "passed" {
			continue
		}
		if check.State == "probing" {
			return true
		}
		check.State, check.StartedAt, check.FinishedAt = "probing", now, time.Time{}
		sampleID := "业务提供的有效样本"
		if len(check.Samples) > 0 {
			sampleID = recoverySampleID(check.Samples[check.ProbeSuccesses%len(check.Samples)])
		}
		e.noteBreakerLocked(s, "stage_probe_started", check.Stage, fmt.Sprintf("开始本阶段第 %d 次只读语义试探，样本=%s；整站保持暂停", check.ProbeSuccesses+1, sampleID), now)
		s.controlRevision = s.event.Revision
		return false
	}
	return false
}

func (e *Engine) launchBreakerStageProbeLocked(ctx context.Context, s *smartBreaker, now time.Time) {
	if s.probeRunning || s.probeExecuting || now.Before(s.event.NextProbeAt) {
		return
	}
	if g := e.Governance(); g != nil {
		if until := g.RetryAfterUntil(s.event.Site); until.After(now) {
			s.event.RetryAfterUntil, s.event.NextProbeAt = until, until
			e.noteBreakerLocked(s, "retry_after_wait", "", "遵守 Retry-After，推迟阶段验证", now)
			return
		}
	}
	if !e.prepareNextBreakerProbeLocked(s, now) {
		return
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State != "probing" {
			continue
		}
		site := s.event.Site
		probe := e.breakerProbes[pauseScope{Site: site, Stage: check.Stage}]
		if probe == nil {
			probe = e.breakerProbes[pauseScope{Site: site}]
		}
		if probe == nil {
			check.State, check.FinishedAt, check.Detail = "missing", now, "没有本阶段语义探针或站点兜底探针"
			e.noteBreakerLocked(s, "stage_probe_missing", check.Stage, check.Detail, now)
			e.transitionBreakerLocked(s, "manual", "存在未验证阶段；保持整站暂停，等待人工处理", now)
			return
		}
		s.probeRunning, s.probeExecuting = true, true
		l := s.event
		req := BreakerProbeRequest{Site: site, Stage: check.Stage, Category: l.report.Category, Evidence: l.reportSnapshot().Evidence, Pass: check.ProbeSuccesses + 1}
		if len(check.Samples) > 0 {
			sample := check.Samples[check.ProbeSuccesses%len(check.Samples)]
			req.SampleID = recoverySampleID(sample)
			req.Sample = &incident.Sample{TaskID: sample.TaskID, Stage: check.Stage, URL: sample.URL}
		}
		if check.Verification == "readonly" {
			req.Coverage = check.Coverage
		}
		e.tickerWG.Add(1)
		go e.runBreakerProbe(ctx, probe, req, l.ID, l.ProbeAttempts, min(s.policy.ProbeTimeout, l.Deadline.Sub(now)))
		return
	}
}

func recoverySampleID(sample breaker.RecoverySample) string {
	if sample.TaskID > 0 {
		return fmt.Sprintf("task:%d", sample.TaskID)
	}
	if sample.URL != "" {
		return "url:" + sample.URL
	}
	return ""
}

// An operator grants a new bounded attempt, not a health verdict. Do not reuse
// old stage proofs or abandon a callback that is still executing.
func (e *Engine) retryBreakerLocked(s *smartBreaker) bool {
	if s.probeRunning || s.probeExecuting {
		return false
	}
	e.beginBreakerRetryLocked(s, time.Now(), "manual_retry", "人工请求重试整站验证；保留失败窗口，重新授予恢复预算")
	return true
}

func (e *Engine) beginBreakerRetryLocked(s *smartBreaker, now time.Time, action, reason string) {
	s.nextSampleRetry = time.Time{}
	s.event.Cycle++
	s.event.RecheckAt = time.Time{}
	e.noteBreakerLocked(s, action, "", reason, now)
	s.trial++
	s.event.ProbeAttempts = 0
	s.event.Deadline = now.Add(s.policy.MaxRecovery)
	s.event.NextProbeAt = now
	e.resetBreakerChecksLocked(s)
	e.transitionBreakerLocked(s, "cooldown", reason, now)
	e.pauseSmartBreakerLocked(s, reason)
}

func (e *Engine) hasManualBreakerPause(site string) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	for scope, state := range e.paused {
		if scope.Site == site && state.Source == PauseSourceManual {
			return true
		}
	}
	return false
}
