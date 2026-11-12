package engine

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"sort"
	"strings"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

func (b *breakerCoordinator) resetBreakerChecksLocked(s *smartBreaker, coverage map[string]string) {
	previous := make(map[string][]breaker.RecoverySample)
	timeouts := make(map[string][]string)
	for _, c := range s.event.Checks {
		previous[c.Stage] = c.Samples
		timeouts[c.Stage] = c.TimedOutIDs
	}
	names := map[string]bool{}
	for stage := range coverage {
		names[stage] = true
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
		if declared := coverage[stage]; strings.TrimSpace(declared) != "" {
			check.Verification, check.Coverage = "readonly", declared
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

func (b *breakerCoordinator) beginBreakerRetryLocked(s *smartBreaker, now time.Time, action, reason string, coverage map[string]string) {
	s.probeSettlement = nil
	s.nextSampleRetry = time.Time{}
	s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
	for i := range s.event.Checks {
		s.event.Checks[i].TimedOutIDs = nil
	}
	s.event.Cycle++
	s.event.RecheckAt = time.Time{}
	b.noteBreakerLocked(s, action, "", reason, now)
	s.trial++
	s.event.ProbeAttempts = 0
	s.event.Deadline = now.Add(s.policy.MaxRecovery)
	s.event.NextProbeAt = now
	b.resetBreakerChecksLocked(s, coverage)
	b.transitionBreakerLocked(s, "cooldown", reason, now)
}

func (b *breakerCoordinator) finishBreakerRecoveryLocked(s *smartBreaker, now time.Time, governance breakerGovernanceEvidence) string {
	if s.event.PendingFailure != nil || s.savedRevision < s.controlRevision {
		return ""
	}
	if len(s.event.pendingHistory) >= 4094 {
		b.transitionBreakerLocked(s, "manual", "历史写入积压，不能完成恢复或清零", now)
		return "历史写入积压"
	}
	if !s.draining {
		s.draining = true
		b.noteBreakerLocked(s, "waiting_inflight", "", fmt.Sprintf("所有阶段验证通过，停止新增放行，等待 %d 个在途任务结束", s.inflight), now)
	}
	if s.inflight != 0 || s.sampleDispatching {
		return ""
	}
	if s.event.SampleID != "" {
		b.noteBreakerLocked(s, "sample_superseded", s.event.SampleStage, "全部阶段已有完整有效证明；结束未执行的样本等待，不额外重投", now)
		s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
	}
	// Freeze pre-reset totals in the report, then start the new site window.
	b.updateBreakerEvidenceLocked(s, s.detector.Snapshot(now), governance)
	s.epoch++
	s.detector.ResetEpoch(s.epoch)
	s.event.CountersResetAt = now
	b.transitionBreakerLocked(s, "recovered", "所有阶段验证通过，全站在途任务已结束；清空运行计数，保留故障历史，报告保存后恢复正常运行", now)
	return ""
}

func (b *breakerCoordinator) advanceSiteLocked(site string, now time.Time, manualPause bool, coverage map[string]string, governance breakerGovernanceEvidence) (pauseReason string, release bool, job *breakerProbeJob) {
	s := b.smartBreakers[site]
	l := s.event
	if l == nil {
		return
	}
	b.expireBreakerSampleLocked(s, now)
	if l.PendingFailure != nil {
		return
	}
	if !l.Active() {
		if s.savedRevision >= s.controlRevision {
			release = true
		}
		return
	}
	if l.State == "manual" {
		if !l.RecheckAt.IsZero() && !now.Before(l.RecheckAt) && s.savedRevision >= s.controlRevision {
			if s.probeRunning || s.probeExecuting || s.inflight > 0 || s.sampleDispatching || manualPause {
				l.RecheckAt = now.Add(time.Hour)
				l.NextProbeAt = l.RecheckAt
				b.noteBreakerLocked(s, "hourly_recheck_skipped", "", "探针/真实任务/重投尚未退出或存在人工暂停；本次不启动复查，一小时后再检查", now)
			} else {
				b.beginBreakerRetryLocked(s, now, "hourly_recheck", "每小时复查：保留故障证据，重新验证全部阶段", coverage)
				pauseReason = "每小时复查：保留故障证据，重新验证全部阶段"
			}
		}
		return
	}
	if !now.Before(l.Deadline) {
		pauseReason = "恢复超时，待人工处理"
		b.transitionBreakerLocked(s, "manual", "恢复时间预算耗尽；不能确认故障已消除", now)
		return
	}
	if s.savedRevision < s.controlRevision {
		return // only durable control changes can enable probes/admission
	}
	if (l.State == "observing" || l.State == "ramping") && breakerSampleTimedOut(s) && l.SampleID == "" && s.inflight == 0 && !s.sampleDispatching {
		_, pending := nextBreakerSample(s)
		if pending == "" {
			b.transitionBreakerLocked(s, "manual", "本周期存在超时样本；其余可用样本已检查，整站保持保护，等待下次复查", now)
			pauseReason = "样本验证超时"
			return
		}
	}
	switch l.State {
	case "observing":
		if now.Sub(l.LastFailureAt) < s.policy.ObserveFor {
			return
		}
		if b.observationRecoveredLocked(s) {
			for i := range s.event.Checks {
				s.event.Checks[i].State = "passed"
				s.event.Checks[i].FinishedAt = now
				s.event.Checks[i].Detail = "本轮观察中不同真实任务持续成功"
			}
			pauseReason = b.finishBreakerRecoveryLocked(s, now, governance)
			return
		}
		_, pendingSample := nextBreakerSample(s)
		if l.SampleID != "" || pendingSample != "" || s.sampleDispatching || breakerSampleTimedOut(s) {
			return // selected sample owns its bounded wait; never start overlapping probes
		}
		s.event.NextProbeAt = now.Add(s.policy.Cooldown)
		pauseReason = "全阶段成功证据不足，暂停整站并安排逐阶段试探"
		b.transitionBreakerLocked(s, "cooldown", "混合观察尚不能证明所有阶段正常；进入整站受控验证", now)
	case "ramping":
		release = true
		if now.Sub(l.UpdatedAt) >= s.policy.RampFor && b.breakerStagesReadyLocked(s, 1) {
			pauseReason = b.finishBreakerRecoveryLocked(s, now, governance)
		}
	case "cooldown":
		if s.probeRunning || now.Before(l.NextProbeAt) {
			return
		}
		if s.inflight > 0 || s.sampleDispatching {
			if !s.draining {
				s.draining = true
				b.noteBreakerLocked(s, "waiting_inflight", "", "等待暂停前已放行的全站任务结束，再进行一致的阶段验证", now)
			}
			return
		}
		if l.ProbeAttempts >= s.policy.MaxProbes {
			b.transitionBreakerLocked(s, "manual", "整站试探轮数耗尽；维持暂停并等待人工处理", now)
			return
		}
		if governance.available {
			until := governance.retryAfter
			if until.After(now) {
				s.event.RetryAfterUntil, s.event.NextProbeAt = until, until
				b.noteBreakerLocked(s, "retry_after_wait", "", "遵守服务端 Retry-After，推迟整站试探", now)
				return
			}
		}
		l.ProbeAttempts++
		b.resetBreakerChecksLocked(s, coverage)
		s.event.NextProbeAt = now
		b.transitionBreakerLocked(s, "probing", "开始一轮整站逐阶段只读语义试探；所有阶段通过前保持暂停", now)
		b.prepareNextBreakerProbeLocked(s, now)
	case "probing":
		job = b.selectStageProbeLocked(s, now, governance.retryAfter)
	}
	return
}

func (b *breakerCoordinator) retryBreakerLocked(s *smartBreaker, coverage map[string]string) bool {
	if s.event.PendingFailure != nil || s.probeRunning || s.probeExecuting || s.inflight > 0 || s.sampleDispatching {
		return false
	}
	b.beginBreakerRetryLocked(s, time.Now(), "manual_retry", "人工请求重试整站验证；保留失败窗口，重新授予恢复预算", coverage)
	return true
}
