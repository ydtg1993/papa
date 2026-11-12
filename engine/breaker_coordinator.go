package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/engine/internal/execution"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/redact"
	"slices"
	"sort"
	"sync"
	"time"
)

// breakerCoordinator owns detector/trial/admission state, recovery evidence,
// history and report-write serialization. It has no Engine reference.
//
// It intentionally remains in package engine: the execution facade coordinates
// breaker -> pause -> overflow locks when publishing protection or settling a
// journaled task result. Moving those transactions behind independent locks
// would split their atomicity. Locked methods require breakerMu; no database
// operation runs under it. flushBreakerReports owns a separate cancellable gate.
type breakerCoordinator struct {
	breakerMu          sync.Mutex
	breakerIncidentSeq uint64
	smartBreakers      map[string]*smartBreaker
	breakerStore       breakerRepository
	breakerStoreGate   chan struct{}
	breakerRuntime     bool
	breakerProbes      map[pauseScope]BreakerProbe
	events             events.Emitter
}

func (b *breakerCoordinator) emit(ev events.Event) {
	if b.events != nil {
		b.events.Emit(ev)
	}
}
func (b *breakerCoordinator) noteBreakerLocked(s *smartBreaker, action, stage, reason string, now time.Time) {
	l := s.event
	b.appendBreakerTransitionLocked(s, incident.Transition{At: now, From: l.State, To: l.State, Action: action, Stage: stage, Round: l.ProbeAttempts, Failures: l.report.WindowFailures, Successes: l.report.WindowSuccesses, NextAttemptAt: l.NextProbeAt, Reason: reason})
	l.Revision++
	s.dirty = true
	b.emit(events.Event{Type: events.TypeBreakerTransition, Site: l.Site, Stage: stage, Attrs: map[string]string{"incident_id": l.ID, "state": l.State, "action": action, "round": fmt.Sprint(l.ProbeAttempts)}})
}

func (b *breakerCoordinator) appendBreakerTransitionLocked(s *smartBreaker, entry incident.Transition) {
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
		for _, check := range l.Checks {
			if check.Stage == entry.Stage {
				copy := breakerCheckReport(check)
				entry.Check = &copy
				break
			}
		}
	}
	// Clean only the new immutable entry; repeatedly cleaning the full history
	// under the admission lock makes multi-stage serial progress unnecessarily slow.
	entry = (incident.Lifecycle{Timeline: []incident.Transition{entry}}).Clean().Timeline[0]
	l.pendingHistory = append(l.pendingHistory, entry)
	l.report.Timeline = append(l.report.Timeline, entry)
	if len(l.report.Timeline) > incident.MaxTransitions {
		l.report.Timeline = l.report.Timeline[len(l.report.Timeline)-incident.MaxTransitions:]
		l.report.Limited = true
	}
}

func (b *breakerCoordinator) recordBreakerStageSuccessLocked(s *smartBreaker, task *Task, now time.Time) {
	if task.breakerTrial != s.trial || task.breakerSampleDeadline.IsZero() || !task.breakerSampleDeadline.Equal(s.event.SampleDeadline) || !now.Before(task.breakerSampleDeadline) {
		return
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.Stage != task.Stage {
			continue
		}
		if slices.Contains(check.TimedOutIDs, recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL})) {
			return
		}
		if s.event.MixedSamples && len(check.Samples) > 0 {
			matched := false
			for j := range check.Samples {
				sample := &check.Samples[j]
				if recoverySampleID(*sample) != recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL}) {
					continue
				}
				need := breakerSampleNeed(len(check.Samples), j)
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
		if s.event.State == "observing" && len(check.TimedOutIDs) == 0 {
			check.State = "observing"
		}
		// First successes and the three-sample proof are milestones; avoid
		// writing one timeline entry for every successful page.
		if check.TaskSuccesses <= 3 {
			b.noteBreakerLocked(s, "stage_task_success", task.Stage, fmt.Sprintf("本轮受控真实任务 %d / 执行轮次 %s 完成；阶段成功不单独解除整站保护", task.ID, task.breakerRound), now)
		}
		return
	}
}

func (b *breakerCoordinator) breakerStagesReadyLocked(s *smartBreaker, minimum int) bool {
	if s.event.ChecksLimited || len(s.event.Checks) == 0 {
		return false
	}
	for _, check := range s.event.Checks {
		if len(check.TimedOutIDs) > 0 {
			return false
		}
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

func (b *breakerCoordinator) prepareNextBreakerProbeLocked(s *smartBreaker, now time.Time) bool {
	if s.event.ChecksLimited || len(s.event.Checks) == 0 {
		b.transitionBreakerLocked(s, "manual", "阶段验证清单为空或超出上限，不能确认整站恢复", now)
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
		b.noteBreakerLocked(s, "stage_probe_started", check.Stage, fmt.Sprintf("开始本阶段第 %d 次只读语义试探，样本=%s；整站保持暂停", check.ProbeSuccesses+1, sampleID), now)
		s.controlRevision = s.event.Revision
		return false
	}
	return false
}

func (b *breakerCoordinator) transitionBreakerLocked(s *smartBreaker, state, reason string, now time.Time) {
	l := s.event
	if state != "probing" {
		for i := range l.Checks {
			c := &l.Checks[i]
			if c.State == "probing" || ((state == "manual" || state == "closed") && c.State == "pending") {
				c.State, c.FinishedAt, c.Detail = "interrupted", now, reason
				b.noteBreakerLocked(s, "stage_probe_interrupted", c.Stage, reason, now)
			}
		}
	}
	previousState := l.State
	l.State, l.UpdatedAt, l.Epoch = state, now, s.epoch
	if state == "manual" && l.RecheckAt.IsZero() {
		l.RecheckAt = now.Add(time.Hour)
	}
	if state == "manual" {
		l.NextProbeAt = l.RecheckAt
	}
	if state == "closed" || state == "recovered" {
		l.RecheckAt = time.Time{}
		l.NextProbeAt = time.Time{}
	}
	b.appendBreakerTransitionLocked(s, incident.Transition{At: now, From: previousState, To: state, Action: state, Reason: reason, Round: l.ProbeAttempts, Failures: l.report.WindowFailures, Successes: l.report.WindowSuccesses, NextAttemptAt: l.NextProbeAt})
	l.Revision++
	s.controlRevision = l.Revision
	s.draining = false
	if state == "observing" || state == "ramping" {
		s.nextSampleRetry = time.Time{}
		s.trial++
		for i := range l.Checks {
			l.Checks[i].TaskSuccesses = 0
			if state == "observing" {
				l.Checks[i].Verification, l.Checks[i].Coverage = "task", "完整真实任务路径；优先失败样本，三次成功；有三个样本时必须不同"
				l.Checks[i].RequiredTasks = 3
			}
		}
	}
	if state != "manual" && state != "closed" && state != "recovered" {
		l.FinalAt, l.report.Conclusion = time.Time{}, ""
	}
	if state == "manual" {
		l.FinalAt = time.Time{}
		l.report.Conclusion = reason + "；继续保护，一小时后复查"
	}
	if state == "closed" || state == "recovered" {
		l.FinalAt, l.report.Conclusion = now, reason
	}
	l.trimReport()
	s.dirty = true
	b.emit(events.Event{Type: events.TypeBreakerTransition, Site: l.Site, Attrs: map[string]string{"incident_id": l.ID, "state": state, "action": state, "round": fmt.Sprint(l.ProbeAttempts)}})
}

func (b *breakerCoordinator) closeSmartBreakerLocked(site, stage, reason string) {
	if s := b.smartBreakers[site]; s != nil {
		if s.event != nil && s.event.Active() {
			b.transitionBreakerLocked(s, "closed", reason+"；人工操作不证明整站健康，不清空计数", time.Now())
		} else if s.event != nil {
			b.noteBreakerLocked(s, "manual_control", stage, reason+"；保留原计数和历史结论", time.Now())
		}
	}
}

func (b *breakerCoordinator) admitBreakerTask(task *Task) (func(), bool) {
	task.breakerSampleDeadline = time.Time{}
	task.breakerSampleFinished = false
	b.breakerMu.Lock()
	s := b.smartBreakers[task.Site]
	if s == nil || (!s.cfg.Enabled && (s.event == nil || s.event.PendingFailure == nil)) {
		b.breakerMu.Unlock()
		return func() {}, true
	}
	now := time.Now()
	if l := s.event; l != nil {
		if l.PendingFailure != nil || s.savedRevision < s.controlRevision || s.draining || (l.Active() && (!now.Before(l.Deadline) || (l.State != "observing" && l.State != "ramping"))) {
			b.breakerMu.Unlock()
			return nil, false
		}
		if l.Active() {
			if now.Before(s.nextAdmission) || !b.admitBreakerSampleLocked(s, task, now) {
				b.breakerMu.Unlock()
				return nil, false
			}
			s.nextAdmission = now.Add(s.policy.Interval)
		}
	}
	task.breakerEpoch = s.epoch
	task.breakerTrial = s.trial
	s.inflight++
	b.breakerMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.breakerMu.Lock()
			defer b.breakerMu.Unlock()
			if l := s.event; l != nil && !task.breakerSampleDeadline.IsZero() && task.breakerSampleDeadline.Equal(l.SampleDeadline) {
				b.expireBreakerSampleLocked(s, time.Now())
				if l.PendingFailure == nil && (task.breakerSampleFinished || !time.Now().Before(task.breakerSampleDeadline)) {
					b.noteBreakerLocked(s, "sample_exited", task.Stage, "串行样本执行已退出；结束占用，不以退出代替验证成功", time.Now())
					l.SampleID, l.SampleStage, l.SampleDeadline = "", "", time.Time{}
				}
				// A denied claim/resource preparation is still the same waiting
				// selection. Retain its original deadline rather than renewing it.
			}
			s.inflight--
		})
	}, true
}

func (b *breakerCoordinator) flushBreakerReports(ctx context.Context) error {
	b.breakerMu.Lock()
	if b.breakerStoreGate == nil {
		b.breakerStoreGate = make(chan struct{}, 1)
	}
	gate := b.breakerStoreGate
	b.breakerMu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	b.breakerMu.Lock()
	store := b.breakerStore
	var pending []breakerstore.Snapshot
	for _, s := range b.smartBreakers {
		if s.dirty && s.event != nil {
			// Window expiry can change the checkpoint without a terminal outcome.
			// A retry after a lost commit acknowledgement needs a fresh revision.
			s.event.Revision++
			pending = append(pending, s.snapshot())
		}
	}
	b.breakerMu.Unlock()
	if store == nil {
		if len(pending) > 0 {
			return errors.New("breaker store is not initialized")
		}
		return nil
	}
	var writeErr error
	for _, l := range pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(writeErr, err)
		}
		if err := store.Save(ctx, l); err != nil {
			writeErr = errors.Join(writeErr, err)
			continue // one unavailable site must not prevent saving the others
		}
		b.breakerMu.Lock()
		if s := b.smartBreakers[l.Control.Site]; s != nil && s.event != nil && s.event.ID == l.Control.ID {
			s.savedRevision = max(s.savedRevision, l.Control.Revision)
			s.dirty = s.event.Revision > s.savedRevision
			kept := s.event.pendingHistory[:0]
			for _, entry := range s.event.pendingHistory {
				if entry.Sequence > l.Control.HistorySequence {
					kept = append(kept, entry)
				}
			}
			s.event.pendingHistory = kept
		}
		b.breakerMu.Unlock()
	}
	return writeErr
}

func (b *breakerCoordinator) observationRecoveredLocked(s *smartBreaker) bool {
	return !s.detector.Snapshot(time.Now()).Limited && b.breakerStagesReadyLocked(s, 3)
}

func (b *breakerCoordinator) recordBreakerStop(drained bool) {
	b.breakerMu.Lock()
	defer b.breakerMu.Unlock()
	now := time.Now()
	for _, s := range b.smartBreakers {
		if s.event == nil {
			continue
		}
		action, reason := "process_stop_incomplete", "进程停止未完全排空；不证明验证任务已完成"
		if drained {
			action, reason = "process_stopped", "进程正常停止；保留验证进度作为历史，重启必须重新验证"
			s.event.StoppedAt = now
		}
		b.noteBreakerLocked(s, action, "", reason, now)
	}
}

func (b *breakerCoordinator) SetBreakerProbe(site, stage string, probe BreakerProbe) {
	b.breakerMu.Lock()
	defer b.breakerMu.Unlock()
	if b.breakerProbes == nil {
		b.breakerProbes = make(map[pauseScope]BreakerProbe)
	}
	b.breakerProbes[pauseScope{Site: site, Stage: stage}] = probe
}

func (b *breakerCoordinator) BreakerReports(ctx context.Context, site string) ([]incident.Lifecycle, error) {
	b.breakerMu.Lock()
	store := b.breakerStore
	live := make(map[string]incident.Lifecycle)
	for key, s := range b.smartBreakers {
		if s.event != nil && (site == "" || site == key) {
			live[s.event.ID] = s.event.reportSnapshot()
		}
	}
	b.breakerMu.Unlock()
	var history []incident.Lifecycle
	if store != nil {
		var err error
		history, err = store.List(ctx, site, 100)
		if err != nil {
			return nil, err
		}
	}
	for _, l := range history {
		if _, ok := live[l.Capture.ID]; !ok {
			live[l.Capture.ID] = l
		}
	}
	out := make([]incident.Lifecycle, 0, len(live))
	for _, l := range live {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (b *breakerCoordinator) selectBreakerSampleLocked(s *smartBreaker, stage, id string, now time.Time) {
	l := s.event
	l.SampleStage, l.SampleID = stage, id
	l.SampleDeadline = now.Add(breakerSampleTimeout)
	if l.Deadline.Before(l.SampleDeadline) {
		l.SampleDeadline = l.Deadline
	}
	s.nextSampleRetry = time.Time{}
	b.noteBreakerLocked(s, "sample_selected", stage, fmt.Sprintf("串行验证选中 %s；等待和执行截止 %s（最长10分钟，受整轮预算限制）", id, l.SampleDeadline.Format(time.RFC3339)), now)
	s.controlRevision = l.Revision
}

func (b *breakerCoordinator) expireBreakerSampleLocked(s *smartBreaker, now time.Time) {
	l := s.event
	if l.SampleID == "" || now.Before(l.SampleDeadline) {
		return
	}
	for i := range l.Checks {
		c := &l.Checks[i]
		if c.Stage != l.SampleStage || slices.Contains(c.TimedOutIDs, l.SampleID) {
			continue
		}
		c.TimedOutIDs = append(c.TimedOutIDs, l.SampleID)
		c.State, c.FinishedAt = "failed", now
		c.Detail = fmt.Sprintf("样本 %s 等待或执行超时；本周期保持未验证，迟到成功作废", l.SampleID)
		if s.inflight > 0 {
			c.Detail += "；执行尚未退出，不能启动下一个样本"
		}
		b.noteBreakerLocked(s, "sample_timeout", c.Stage, c.Detail, now)
		s.controlRevision = l.Revision
	}
	// Neither SQL still submitting nor a handler ignoring cancellation may
	// release the site's serial slot early.
	if s.inflight == 0 && !s.sampleDispatching && l.PendingFailure == nil {
		l.SampleID, l.SampleStage, l.SampleDeadline = "", "", time.Time{}
	}
}

func (b *breakerCoordinator) admitBreakerSampleLocked(s *smartBreaker, task *Task, now time.Time) bool {
	l := s.event
	if s.event.PendingFailure != nil || s.inflight != 0 || s.sampleDispatching || s.probeRunning || s.probeExecuting {
		return false
	}
	b.expireBreakerSampleLocked(s, now)
	if l.SampleID == "" {
		stage, id := nextBreakerSample(s)
		if id == "" {
			id = recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL})
			if id == "" {
				return false
			}
			eligible := false
			for _, c := range l.Checks {
				if c.Stage != task.Stage || slices.Contains(c.TimedOutIDs, id) || len(c.TimedOutIDs) >= 3 {
					continue
				}
				eligible = !l.MixedSamples || (len(c.Samples) == 0 && c.Verification != "readonly" && c.TaskSuccesses < c.RequiredTasks && !slices.Contains(c.SuccessIDs, id))
			}
			if !eligible {
				return false
			}
			stage = task.Stage
		}
		b.selectBreakerSampleLocked(s, stage, id, now)
		return false // persist selection/deadline before any task can run
	}
	if s.savedRevision < s.controlRevision || !now.Before(l.SampleDeadline) || l.SampleStage != task.Stage || l.SampleID != recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL}) {
		return false
	}
	task.breakerSampleDeadline = l.SampleDeadline
	return true
}

func (b *breakerCoordinator) breakerSettlementBlocked(site string) bool {
	b.breakerMu.Lock()
	defer b.breakerMu.Unlock()
	s := b.smartBreakers[site]
	return s != nil && s.event != nil && (s.event.PendingFailure != nil || s.savedRevision < s.controlRevision)
}

func (b *breakerCoordinator) retainFailure(ctx context.Context, task *Task, version taskRunVersion, cause error, unresolved *execution.State) error {
	b.breakerMu.Lock()
	s := b.smartBreakers[task.Site]
	if s == nil || s.event == nil {
		b.breakerMu.Unlock()
		return errors.New("serial failure has no recovery state")
	}
	if p := s.event.PendingFailure; p != nil {
		if p.TaskID != task.ID || p.Repeat != version.Repeat || p.Reprocess != version.Reprocess {
			b.breakerMu.Unlock()
			return errors.New("another serial failure is awaiting settlement")
		}
	} else {
		f := incident.ClassifyError(cause)
		kind, policy := failureCompensation(cause)
		s.event.PendingFailure = &breaker.PendingFailure{TaskID: task.ID, Stage: task.Stage, URL: task.URL,
			Repeat: version.Repeat, Reprocess: version.Reprocess, At: time.Now(), SampleDeadline: task.breakerSampleDeadline,
			Error: redact.Text(cause.Error()), Category: f.Category, Fingerprint: f.Fingerprint,
			FailureKind: kind, CompensationMode: string(policy.Mode), CompensationReason: policy.Reason, NotBefore: policy.NotBefore}
		for i := range s.event.Checks {
			if s.event.Checks[i].Stage == task.Stage {
				s.event.Checks[i].State = "failed"
				s.event.Checks[i].Detail = "真实失败待落库确认；不能继续验证或当作健康证明：" + redact.Text(cause.Error())
			}
		}
		b.noteBreakerLocked(s, "failure_pending", task.Stage, fmt.Sprintf("任务 %d 轮次 %d:%d 真实失败待落库确认；阻止后续验证：%s", task.ID, version.Repeat, version.Reprocess, cause), time.Now())
		s.controlRevision = s.event.Revision
	}
	b.breakerMu.Unlock()
	unresolved.Remember(task.ID, stageKeyOf(task))
	return b.flushBreakerReports(ctx)
}
