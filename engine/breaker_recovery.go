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
)

// The registry defines coverage, including stages that have not run yet. The
// evidence fallback supports direct Engine users without a stage registry.
func (e *Engine) resetBreakerChecksLocked(s *smartBreaker) {
	previous := make(map[string][]breaker.RecoverySample)
	timeouts := make(map[string][]string)
	for _, c := range s.event.Checks {
		previous[c.Stage] = c.Samples
		timeouts[c.Stage] = c.TimedOutIDs
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
		check.TimedOutIDs = timeouts[stage]
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

func (e *Engine) finishBreakerRecoveryLocked(s *smartBreaker, now time.Time) {
	if s.event.PendingFailure != nil || s.savedRevision < s.controlRevision {
		return
	}
	if len(s.event.pendingHistory) >= 4094 {
		e.breakers.transitionBreakerLocked(s, "manual", "历史写入积压，不能完成恢复或清零", now)
		e.pauseSmartBreakerLocked(s, "历史写入积压")
		return
	}
	if !s.draining {
		s.draining = true
		e.breakers.noteBreakerLocked(s, "waiting_inflight", "", fmt.Sprintf("所有阶段验证通过，停止新增放行，等待 %d 个在途任务结束", s.inflight), now)
	}
	if s.inflight != 0 || s.sampleDispatching {
		return
	}
	if s.event.SampleID != "" {
		e.breakers.noteBreakerLocked(s, "sample_superseded", s.event.SampleStage, "全部阶段已有完整有效证明；结束未执行的样本等待，不额外重投", now)
		s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
	}
	// Freeze pre-reset totals in the report, then start the new site window.
	e.updateBreakerEvidenceLocked(s, s.detector.Snapshot(now))
	s.epoch++
	s.detector.ResetEpoch(s.epoch)
	s.event.CountersResetAt = now
	e.breakers.transitionBreakerLocked(s, "recovered", "所有阶段验证通过，全站在途任务已结束；清空运行计数，保留故障历史，报告保存后恢复正常运行", now)
}

func (e *Engine) launchBreakerStageProbeLocked(ctx context.Context, s *smartBreaker, now time.Time) {
	if s.probeRunning || s.probeExecuting {
		return
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State == "probing" && !now.Before(check.StartedAt.Add(breakerSampleTimeout)) {
			check.State, check.FinishedAt, check.Detail = "failed", now, "只读样本选中后等待超过10分钟；未执行，不证明健康"
			e.breakers.noteBreakerLocked(s, "stage_probe_timeout", check.Stage, check.Detail, now)
			e.breakers.transitionBreakerLocked(s, "manual", check.Detail, now)
			return
		}
	}
	if now.Before(s.event.NextProbeAt) {
		return
	}
	if g := e.Governance(); g != nil {
		if until := g.RetryAfterUntil(s.event.Site); until.After(now) {
			s.event.RetryAfterUntil, s.event.NextProbeAt = until, until
			e.breakers.noteBreakerLocked(s, "retry_after_wait", "", "遵守 Retry-After，推迟阶段验证", now)
			return
		}
	}
	if !e.breakers.prepareNextBreakerProbeLocked(s, now) {
		return
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State != "probing" {
			continue
		}
		site := s.event.Site
		probe := e.breakers.breakerProbes[pauseScope{Site: site, Stage: check.Stage}]
		if probe == nil {
			probe = e.breakers.breakerProbes[pauseScope{Site: site}]
		}
		if probe == nil {
			check.State, check.FinishedAt, check.Detail = "missing", now, "没有本阶段语义探针或站点兜底探针"
			e.breakers.noteBreakerLocked(s, "stage_probe_missing", check.Stage, check.Detail, now)
			e.breakers.transitionBreakerLocked(s, "manual", "存在未验证阶段；保持整站暂停，等待人工处理", now)
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
		go e.runBreakerProbe(ctx, probe, req, l.ID, l.ProbeAttempts, min(s.policy.ProbeTimeout, l.Deadline.Sub(now), check.StartedAt.Add(breakerSampleTimeout).Sub(now)))
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
	if s.event.PendingFailure != nil || s.probeRunning || s.probeExecuting || s.inflight > 0 || s.sampleDispatching {
		return false
	}
	e.beginBreakerRetryLocked(s, time.Now(), "manual_retry", "人工请求重试整站验证；保留失败窗口，重新授予恢复预算")
	return true
}

func (e *Engine) beginBreakerRetryLocked(s *smartBreaker, now time.Time, action, reason string) {
	s.nextSampleRetry = time.Time{}
	s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
	for i := range s.event.Checks {
		s.event.Checks[i].TimedOutIDs = nil
	}
	s.event.Cycle++
	s.event.RecheckAt = time.Time{}
	e.breakers.noteBreakerLocked(s, action, "", reason, now)
	s.trial++
	s.event.ProbeAttempts = 0
	s.event.Deadline = now.Add(s.policy.MaxRecovery)
	s.event.NextProbeAt = now
	e.resetBreakerChecksLocked(s)
	e.breakers.transitionBreakerLocked(s, "cooldown", reason, now)
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
