package engine

import (
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/pkg/incident"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

// breakerGovernanceEvidence is a point-in-time input, never a live governor.
type breakerGovernanceEvidence struct {
	available    bool
	autoThrottle bool
	retryAfter   time.Time
}

func (b *breakerCoordinator) updateBreakerEvidenceLocked(s *smartBreaker, d breaker.Decision, governance breakerGovernanceEvidence) {
	l := s.event
	l.report.WindowFailures, l.report.WindowSuccesses = d.Failures, d.Successes
	l.report.ShortFailures, l.report.DistinctTasks = d.ShortFailures, d.DistinctTasks
	// Retain the last classification after short-window faults are acknowledged.
	if len(d.Groups) > 0 {
		l.report.Groups = nil
		for _, group := range d.Groups {
			l.report.Groups = append(l.report.Groups, incident.FailureGroup{Stage: group.Stage, Category: group.Category, Fingerprint: group.Fingerprint, Count: group.Count, Tasks: group.Tasks})
		}
	}
	l.report.Limited = l.report.Limited || d.Limited
	if governance.available {
		l.report.AutoThrottle = governance.autoThrottle
		if governance.retryAfter.After(l.RetryAfterUntil) {
			l.RetryAfterUntil = governance.retryAfter
		}
	}
	l.Revision++
	s.dirty = true
}

func (b *breakerCoordinator) recordSmartOutcomeLocked(task *Task, err error, coverage map[string]string, governance breakerGovernanceEvidence) (pauseReason string, notify bool) {
	s := b.smartBreakers[task.Site]
	if s == nil || !s.cfg.Enabled || task.breakerEpoch != s.epoch {
		return pauseReason, false
	}
	now := time.Now()
	task.breakerSampleFinished = true // called only for a confirmed terminal task round
	lateSample := s.event != nil && !task.breakerSampleDeadline.IsZero() && !now.Before(task.breakerSampleDeadline)
	if lateSample {
		b.expireBreakerSampleLocked(s, now)
	}
	f := incident.ClassifyError(err)
	outcomeAt := now
	settling := false
	if s.event != nil && s.event.PendingFailure != nil && err != nil {
		p := s.event.PendingFailure
		if p.TaskID == task.ID && task.breakerRound == fmt.Sprintf("%d:%d", p.Repeat, p.Reprocess) {
			settling = true
			err = errors.New(p.Error) // the journaled business result remains authoritative
			f.Category, f.Fingerprint, outcomeAt = p.Category, p.Fingerprint, p.At
			defer func() {
				s.event.PendingFailure = nil
				b.noteBreakerLocked(s, "failure_settled", task.Stage, fmt.Sprintf("任务 %d 轮次 %s 失败终态已确认；按原发生时间和轮次幂等结算，已过窗口的失败不补入当前窗口；保存后才允许后续验证", task.ID, task.breakerRound), now)
				s.controlRevision = s.event.Revision
			}()
		}
	}
	sample := incident.Sample{At: outcomeAt, TaskID: int64(task.ID), Stage: task.Stage, URL: task.URL, Kind: f.Category}
	if err != nil {
		sample.Error = err.Error()
	}
	sample = incident.Clean(sample)
	if settling {
		s.detector.Snapshot(now) // advance wall time before evaluating a delayed result
	}
	d := s.detector.Observe(breaker.Outcome{TaskID: int64(task.ID), Round: task.breakerRound, Site: task.Site, Stage: task.Stage, URL: sample.URL, At: outcomeAt, Success: err == nil, Category: f.Category, Fingerprint: f.Fingerprint, Epoch: s.epoch, Error: sample.Error, Kind: sample.Kind})
	if !d.Accepted {
		if settling {
			// An old result can be confirmed after its statistical window has
			// expired. Report the confirmation without reviving old counts or
			// leaving the stage displayed as awaiting persistence.
			b.updateBreakerEvidenceLocked(s, s.detector.Snapshot(now), governance)
			for i := range s.event.Checks {
				if s.event.Checks[i].Stage == task.Stage {
					s.event.Checks[i].State = "failed"
					s.event.Checks[i].Detail = "实际失败已落库；原轮次已计数或已超过统计窗口：" + sample.Error
				}
			}
			b.noteBreakerLocked(s, "task_failed", task.Stage, "原失败终态已确认，不重复或补入当前窗口："+sample.Error, now)
		}
		return pauseReason, false
	}
	if s.event != nil && (s.event.Active() || s.event.PendingFailure != nil) {
		b.updateBreakerEvidenceLocked(s, d, governance)
	}
	if lateSample {
		b.noteBreakerLocked(s, "sample_late_result", task.Stage, "记录超时后的实际任务终态；迟到成功不能补算健康证明，实际失败仍进入失败统计", now)
		if err == nil {
			return pauseReason, false
		}
	}
	if err == nil {
		if s.event != nil && (s.event.State == "ramping" || s.event.State == "observing") {
			s.event.Successes++
			b.recordBreakerStageSuccessLocked(s, task, now)
		}
		return pauseReason, false
	}
	if s.event != nil && (s.event.Active() || s.event.PendingFailure != nil || (s.dirty && d.Rule != "")) {
		if !s.event.Active() {
			b.updateBreakerEvidenceLocked(s, d, governance)
		}
		s.event.LastFailureAt = now
		if s.event.State == "observing" {
			s.draining = false
			s.event.Successes = 0
			s.trial++
			for i := range s.event.Checks {
				if s.event.Checks[i].Stage == task.Stage && len(s.event.Checks[i].Samples) < 3 {
					sample := breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL}
					exists := false
					for _, prior := range s.event.Checks[i].Samples {
						if recoverySampleID(prior) == recoverySampleID(sample) {
							exists = true
						}
					}
					if !exists && recoverySampleID(sample) != "" {
						s.event.Checks[i].Samples = append(s.event.Checks[i].Samples, sample)
					}
				}
				s.event.Checks[i].TaskSuccesses = 0
				s.event.Checks[i].SuccessIDs = nil
				for j := range s.event.Checks[i].Samples {
					s.event.Checks[i].Samples[j].Successes = 0
				}
			}
		}
		s.event.report.Evidence = append(s.event.report.Evidence, sample)
		s.event.Revision++
		s.dirty = true
		s.event.trimReport()
		for i := range s.event.Checks {
			if s.event.Checks[i].Stage == task.Stage {
				s.event.Checks[i].State, s.event.Checks[i].Detail = "failed", sample.Error
			}
		}
		b.noteBreakerLocked(s, "task_failed", task.Stage, "真实任务终态失败："+sample.Error, now)
		if s.event.State == "ramping" || s.event.State == "probing" {
			if d.Rule == "hard" || d.Rule == "capacity" {
				s.event.Stage, s.event.report.Rule = "", d.Rule
			}
			s.event.NextProbeAt = now.Add(s.policy.ProbeInterval * time.Duration(1<<min(s.event.ProbeAttempts, 6)))
			b.transitionBreakerLocked(s, "cooldown", "验证期间再次失败；全部阶段需重新验证", now)
			pauseReason = "限量恢复失败，重新冷却"
		} else if d.Rule == "hard" || d.Rule == "capacity" || d.Rule == "fast" || !s.event.Active() {
			s.event.Stage = ""
			s.event.report.Rule = d.Rule
			if s.event.State != "manual" && (s.event.State == "observing" || s.event.State == "ramping" || s.event.State == "probing" || !s.event.Active()) {
				b.transitionBreakerLocked(s, "cooldown", "观察期间达到快速或总数保护线", now)
				s.event.NextProbeAt = now.Add(s.policy.Cooldown)
			}
			pauseReason = "分类熔断升级保护"
		}
		return pauseReason, false
	}
	if d.Rule == "" || s.dirty {
		return pauseReason, false
	}
	b.breakerIncidentSeq++
	report := incident.Lifecycle{
		Capture: incident.Capture{ID: fmt.Sprintf("%d-%d", now.UnixNano(), b.breakerIncidentSeq), Site: task.Site, At: now, From: now.Add(-s.cfg.Window), Window: s.cfg.Window, Threshold: s.cfg.Threshold, Failures: d.Failures},
		Stage:   "", Rule: d.Rule, Category: f.Category, Fingerprint: f.Fingerprint, Epoch: s.epoch,
		StartedAt: now, UpdatedAt: now, Deadline: now.Add(s.policy.MaxRecovery), NextProbeAt: now.Add(s.policy.Cooldown), Limited: d.Limited,
		LastFailureAt: now,
		Hypotheses:    []string{"按错误类别与指纹聚合，尚未证明整站宕机"},
		Missing:       []string{"需要只读语义探测验证故障操作；请求成功与任务成功分别判断"},
	}
	evidence, limited := s.detector.Evidence(now, incident.MaxSamples)
	report.Limited = report.Limited || limited
	for _, o := range evidence {
		report.Evidence = append(report.Evidence, incident.Sample{At: o.At, TaskID: o.TaskID, Stage: o.Stage, URL: o.URL, Kind: o.Kind, Error: o.Error})
	}

	l := &breakerEvent{Recovery: breaker.Recovery{Version: 1, ID: report.Capture.ID, Site: task.Site, State: report.State, Epoch: s.epoch, StartedAt: now, UpdatedAt: now, Deadline: report.Deadline, NextProbeAt: report.NextProbeAt, LastFailureAt: now}, report: report}
	s.event = l
	l.MixedSamples = d.Rule == "mixed"
	l.Cycle = 1
	b.resetBreakerChecksLocked(s, coverage)
	s.savedRevision = 0
	b.updateBreakerEvidenceLocked(s, d, governance)
	state, reason := "cooldown", "同类短时爆发或窗口失败总数达到保护线"
	if d.Rule == "mixed" {
		state, reason = "observing", "混合错误短时增多，限制并发并观察"
	}
	b.transitionBreakerLocked(s, state, reason, now)
	if state == "observing" {
		return pauseReason, false
	}
	return reason, true
}
