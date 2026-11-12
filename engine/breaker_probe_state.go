package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"strings"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

// breakerProbeJob transfers invocation to the Engine lifecycle with copied
// request evidence and a bounded deadline; it carries no live event pointer.
type breakerProbeJob struct {
	probe   BreakerProbe
	request BreakerProbeRequest
	id      string
	round   int
	timeout time.Duration
}

func (b *breakerCoordinator) selectStageProbeLocked(s *smartBreaker, now, retryAfter time.Time) *breakerProbeJob {
	if s.probeRunning || s.probeExecuting || s.probeSettlement != nil {
		return nil
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State == "probing" && !now.Before(check.StartedAt.Add(breakerSampleTimeout)) {
			check.State, check.FinishedAt, check.Detail = "failed", now, "只读样本选中后等待超过10分钟；未执行，不证明健康"
			b.noteBreakerLocked(s, "stage_probe_timeout", check.Stage, check.Detail, now)
			b.transitionBreakerLocked(s, "manual", check.Detail, now)
			return nil
		}
	}
	if now.Before(s.event.NextProbeAt) {
		return nil
	}
	if retryAfter.After(now) {
		s.event.RetryAfterUntil, s.event.NextProbeAt = retryAfter, retryAfter
		b.noteBreakerLocked(s, "retry_after_wait", "", "遵守 Retry-After，推迟阶段验证", now)
		return nil
	}
	if !b.prepareNextBreakerProbeLocked(s, now) {
		return nil
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.State != "probing" {
			continue
		}
		site := s.event.Site
		probe := b.breakerProbes[pauseScope{Site: site, Stage: check.Stage}]
		if probe == nil {
			probe = b.breakerProbes[pauseScope{Site: site}]
		}
		if probe == nil {
			check.State, check.FinishedAt, check.Detail = "missing", now, "没有本阶段语义探针或站点兜底探针"
			b.noteBreakerLocked(s, "stage_probe_missing", check.Stage, check.Detail, now)
			b.transitionBreakerLocked(s, "manual", "存在未验证阶段；保持整站暂停，等待人工处理", now)
			return nil
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
		return &breakerProbeJob{probe: probe, request: req, id: l.ID, round: l.ProbeAttempts, timeout: min(s.policy.ProbeTimeout, l.Deadline.Sub(now), check.StartedAt.Add(breakerSampleTimeout).Sub(now))}
	}
	return nil
}

// breakerProbeSettlement fences the second phase across external milestone
// callbacks. Its state pointers are private to the coordinator.
type breakerProbeSettlement struct {
	state           *smartBreaker
	event           *breakerEvent
	site, stage, id string
	round           int
	epoch, trial    uint64
	checkState      string
	successes       int
	healthy         bool
	resultErr       error
}

// settleStageProbeLocked records the result milestone, without making the final
// recovery transition. The caller publishes its events before finishing below.
func (b *breakerCoordinator) settleStageProbeLocked(req BreakerProbeRequest, id string, round int, result BreakerProbeResult, resultErr error, probeCtx context.Context) (*breakerProbeSettlement, string) {
	s := b.smartBreakers[req.Site]
	if s == nil || s.event == nil || s.event.ID != id || s.event.ProbeAttempts != round {
		return nil, ""
	}
	s.probeRunning = false
	if s.event.State != "probing" {
		return nil, ""
	}
	now := time.Now()
	if !now.Before(s.event.Deadline) {
		b.transitionBreakerLocked(s, "manual", "恢复预算已耗尽，晚返回的探针不能证明按时恢复", now)
		return nil, "恢复超时，待人工处理"
	}
	if probeCtx.Err() != nil {
		resultErr = probeCtx.Err()
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.Stage != req.Stage || check.State != "probing" {
			continue
		}
		check.FinishedAt, check.Detail = now, result.Detail
		complete := (check.Verification != "readonly" || (result.Complete && strings.TrimSpace(result.Detail) != "")) && (req.SampleID == "" || req.SampleID == result.SampleID)
		healthy := resultErr == nil && result.Healthy && result.Semantic && complete
		if healthy {
			check.ProbeSuccesses++
			check.State = "passed"
			if check.ProbeSuccesses < check.RequiredProbes {
				check.State = "pending"
			}
			s.event.NextProbeAt = now.Add(s.policy.Interval)
			b.noteBreakerLocked(s, "stage_probe_passed", req.Stage, fmt.Sprintf("本阶段第 %d 次语义试探通过，指定样本=%s：%s", check.ProbeSuccesses, req.SampleID, result.Detail), now)
		} else {
			check.State = "failed"
			reason := "本阶段语义试探未通过，指定样本=" + req.SampleID + "：" + result.Detail
			if !complete {
				reason = "检查未确认完整覆盖、缺少说明或未确认指定样本；阶段仍未验证：" + result.Detail
			}
			if resultErr != nil {
				reason += " " + resultErr.Error()
			}
			check.Detail = reason
			s.event.NextProbeAt = now.Add(s.policy.ProbeInterval * time.Duration(1<<min(s.event.ProbeAttempts, 6)))
			b.noteBreakerLocked(s, "stage_probe_failed", req.Stage, reason, now)
		}
		token := &breakerProbeSettlement{state: s, event: s.event, site: req.Site, stage: req.Stage, id: id, round: round, epoch: s.epoch, trial: s.trial, checkState: check.State, successes: check.ProbeSuccesses, healthy: healthy, resultErr: resultErr}
		s.probeSettlement = token
		return token, ""
	}
	return nil, ""
}

func (b *breakerCoordinator) finishStageProbeLocked(token *breakerProbeSettlement, parentCtx context.Context) string {
	s := b.smartBreakers[token.site]
	if s != token.state || s.probeSettlement != token {
		return ""
	}
	s.probeSettlement = nil
	if s.event != token.event || s.event.ID != token.id || s.event.State != "probing" || s.event.ProbeAttempts != token.round || s.epoch != token.epoch || s.trial != token.trial {
		return ""
	}
	matched := false
	all := true
	for _, check := range s.event.Checks {
		all = all && check.State == "passed"
		if check.Stage == token.stage {
			matched = check.State == token.checkState && check.ProbeSuccesses == token.successes
		}
	}
	if !matched {
		return ""
	}
	now := time.Now()
	if !now.Before(s.event.Deadline) {
		b.transitionBreakerLocked(s, "manual", "恢复预算已耗尽，晚返回的探针不能证明按时恢复", now)
		return "恢复超时，待人工处理"
	}
	if token.healthy {
		// The milestone callback may synchronously stop or cancel recovery.
		if all && parentCtx.Err() == nil {
			s.event.Successes = 0
			b.transitionBreakerLocked(s, "ramping", "所有阶段语义试探通过；进入整站限量观察，未声明完整只读覆盖的阶段仍须真实任务验证，尚不清空计数", now)
		}
		return ""
	}
	if parentCtx.Err() != nil {
		b.transitionBreakerLocked(s, "cooldown", "进程停止；保存阶段结果，重启后重新验证所有阶段", now)
	} else if errors.Is(token.resultErr, context.DeadlineExceeded) {
		b.transitionBreakerLocked(s, "manual", "阶段探针超时；停止自动试探，保持整站暂停", now)
	} else {
		b.transitionBreakerLocked(s, "cooldown", "阶段试探失败；整站继续暂停，等待下一轮重试", now)
	}
	return ""
}

func (b *breakerCoordinator) probeExited(site string) {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if s := b.smartBreakers[site]; s != nil {
		s.probeExecuting = false
	}
}

// abandonStageProbeSettlement releases only this result's callback reservation.
// If external Emit panics and its caller recovers, no process-local token may
// permanently prevent the remaining stages from being probed.
func (b *breakerCoordinator) abandonStageProbeSettlement(token *breakerProbeSettlement) {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if s := b.smartBreakers[token.site]; s == token.state && s.probeSettlement == token {
		s.probeSettlement = nil
	}
}
