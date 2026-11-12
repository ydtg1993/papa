package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// BreakerProbe must perform a bounded, read-only health check, never a FetchHandler
// or business write. Healthy and Semantic must both be true to reopen admission:
// an HTTP 200 alone does not prove that the failing page operation works again.
type BreakerProbe func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error)
type BreakerProbeRequest struct {
	Site, Stage, Category string
	Evidence              []incident.Sample
	Pass                  int    // 1-based independent check within this stage's recovery round
	Coverage              string // declared complete read-only coverage; empty for ordinary probes
	SampleID              string // selected failed task identity; echo in result after checking this sample
	Sample                *incident.Sample
}
type BreakerProbeResult struct {
	Healthy, Semantic bool
	Complete          bool // this call fully checked the declared read-only scope, not HTTP status alone
	Detail            string
	SampleID          string
}

type breakerRepository interface {
	Save(context.Context, breakerstore.Snapshot) error
	LoadRuntime(context.Context) ([]breakerstore.Snapshot, error)
	List(context.Context, string, int) ([]incident.Lifecycle, error)
}
type smartBreaker struct {
	cfg             breaker.Config
	policy          breaker.Policy
	detector        *breaker.Detector
	epoch           uint64
	event           *breakerEvent
	dirty           bool
	controlRevision uint64 // revision that must be saved before releasing protection
	savedRevision   uint64
	draining        bool // stop new admissions while settling the current recovery cohort
	inflight        int
	nextAdmission   time.Time
	probeRunning    bool
	probeExecuting  bool
	manualTakeover  bool   // process-local; ordinary resume must still revalidate
	trial           uint64 // admission generation for controlled real-task evidence
	nextSampleRetry time.Time
}

// SetBreakerProbe registers a semantic probe before Run. Empty stage is a site
// fallback. The context carries the existing request governor and site identity.
func (e *Engine) SetBreakerProbe(site, stage string, probe BreakerProbe) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if e.breakerProbes == nil {
		e.breakerProbes = make(map[pauseScope]BreakerProbe)
	}
	e.breakerProbes[pauseScope{Site: site, Stage: stage}] = probe
}

// InitBreakerRuntime restores protection before entry submission/recovery queues.
// Schema migration remains an explicit operation; an unreadable store stops startup.
func (e *Engine) InitBreakerRuntime(ctx context.Context) error {
	e.breakerMu.Lock()
	if e.breakerRuntime {
		e.breakerMu.Unlock()
		return nil
	}
	if e.breakerStore == nil {
		e.breakerStore = breakerstore.New(e.db)
	}
	store := e.breakerStore
	e.breakerMu.Unlock()
	active, err := store.LoadRuntime(ctx)
	if err != nil {
		return err
	}
	e.breakerMu.Lock()
	now := time.Now()
	for _, saved := range active {
		s := e.smartBreakers[saved.Control.Site]
		if s == nil {
			continue
		} // retain the durable report for unregistered sites
		if s.event != nil && s.event.ID != saved.Control.ID {
			e.breakerMu.Unlock()
			return fmt.Errorf("multiple active breaker incidents for site %q", saved.Control.Site)
		}
		copy := saved.Control
		s.event = &breakerEvent{Recovery: copy, report: saved.Lifecycle.Clean()}
		if g := e.Governance(); g != nil {
			g.RestoreRetryAfter(saved.Control.Site, saved.Control.RetryAfterUntil)
		}
		s.savedRevision = saved.Control.Revision
		s.controlRevision = saved.Control.Revision
		s.epoch = saved.Control.Epoch + 1
		if saved.Window != nil {
			s.detector.Restore(*saved.Window, s.epoch, now)
		} else {
			until := saved.LastFailureAt.Add(s.cfg.Window)
			s.detector.Restore(breaker.Checkpoint{LegacyFailures: saved.WindowFailures, LegacyUntil: until}, s.epoch, now)
			if copy.Active() {
				copy.State = "manual"
			}
		}
		if !copy.Active() {
			continue
		}
		if copy.HistorySequence == 0 {
			e.noteBreakerLocked(s, "history_start", "", "开始追加式历史；升级前仅有当前快照，无法补全更早过程", now)
		}
		// Freeze the old declaration/results in history before rebuilding control.
		for _, check := range s.event.Checks {
			e.noteBreakerLocked(s, "previous_stage_snapshot", check.Stage, "重启前最后保存的阶段验证；仅留作历史，不复用健康证明", now)
		}
		if copy.StoppedAt.IsZero() {
			e.noteBreakerLocked(s, "process_interrupted", "", "启动时发现上次运行未保存正常停止记录；确切中断时间未知，旧验证作废", now)
		} else {
			e.noteBreakerLocked(s, "process_restart", "", "正常停止后重新启动；保留此前过程，旧验证作废", now)
		}
		s.event.StoppedAt = time.Time{}
		s.event.Stage = "" // active older stage-scoped incidents protect the whole site
		e.resetBreakerChecksLocked(s)
		if saved.Window == nil {
			e.noteBreakerLocked(s, "legacy_window_missing", "", "旧快照没有精确失败窗口，保留已知失败下界并等待人工重试验证", now)
		}
		if copy.State != "manual" {
			e.transitionBreakerLocked(s, "cooldown", "进程重启：恢复暂停后重新探测", now)
			if s.event.NextProbeAt.Before(now) {
				s.event.NextProbeAt = now.Add(s.policy.Cooldown)
			}
		} else {
			e.transitionBreakerLocked(s, "manual", "进程重启：保留整站保护，阶段验证需重新执行", now)
		}
		e.pauseSmartBreakerLocked(s, "恢复持久化熔断事件")
	}
	e.breakerRuntime = true
	e.breakerMu.Unlock()
	if err := e.flushBreakerReports(ctx); err != nil {
		e.breakerMu.Lock()
		e.breakerRuntime = false
		e.breakerMu.Unlock()
		return err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	stop := func() bool { return false }
	if e.ctx != nil {
		stop = context.AfterFunc(e.ctx, cancel)
	}
	e.tickerWG.Add(2)
	go e.writeBreakerReports(loopCtx)
	go func() {
		defer e.tickerWG.Done()
		defer cancel()
		defer stop()
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case now := <-timer.C:
				e.tickBreakers(loopCtx, now)
				timer.Reset(200 * time.Millisecond)
			}
		}
	}()
	return nil
}

func (e *Engine) recordBreakerStop(drained bool) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	now := time.Now()
	for _, s := range e.smartBreakers {
		if s.event == nil {
			continue
		}
		action, reason := "process_stop_incomplete", "进程停止未完全排空；不证明验证任务已完成"
		if drained {
			action, reason = "process_stopped", "进程正常停止；保留验证进度作为历史，重启必须重新验证"
			s.event.StoppedAt = now
		}
		e.noteBreakerLocked(s, action, "", reason, now)
	}
}

// Both healthy and failed writes wait on a timer. Coalescing has a fixed maximum
// wait instead of restarting a debounce timer for every terminal task.
func (e *Engine) writeBreakerReports(ctx context.Context) {
	defer e.tickerWG.Done()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := e.flushBreakerReports(writeCtx)
			if err == nil {
				e.retryBreakerSamples(writeCtx)
			}
			cancel()
			delay := 200 * time.Millisecond
			if err != nil {
				delay = time.Second
				if e.loggerSet != nil {
					e.loggerSet.Engine.Errorf("persist breaker report: %v", err)
				}
			}
			timer.Reset(delay)
		}
	}
}

func (e *Engine) transitionBreakerLocked(s *smartBreaker, state, reason string, now time.Time) {
	l := s.event
	if state != "probing" {
		for i := range l.Checks {
			c := &l.Checks[i]
			if c.State == "probing" || ((state == "manual" || state == "closed") && c.State == "pending") {
				c.State, c.FinishedAt, c.Detail = "interrupted", now, reason
				e.noteBreakerLocked(s, "stage_probe_interrupted", c.Stage, reason, now)
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
	}
	e.appendBreakerTransitionLocked(s, incident.Transition{At: now, From: previousState, To: state, Action: state, Reason: reason, Round: l.ProbeAttempts, Failures: l.report.WindowFailures, Successes: l.report.WindowSuccesses, NextAttemptAt: l.NextProbeAt})
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
	e.emit(events.Event{Type: events.TypeBreakerTransition, Site: l.Site, Attrs: map[string]string{"incident_id": l.ID, "state": state, "action": state, "round": fmt.Sprint(l.ProbeAttempts)}})
}

func (e *Engine) pauseSmartBreakerLocked(s *smartBreaker, reason string) bool {
	l := s.event
	stage := l.Stage
	if stage == "" && len(l.report.Evidence) > 0 {
		stage = l.report.Evidence[len(l.report.Evidence)-1].Stage
	}
	return e.setPause(pauseScope{Site: l.Site, Stage: l.Stage}, pauseState{
		Reason: reason, Source: PauseSourceBreaker, Owner: l.ID,
		Stage: stage, Failures: l.report.Capture.Failures,
	})
}

// Only this incident's automatic pauses can be released. Manual pauses and
// other owners survive, including manual stage pauses beneath an automatic site pause.
func (e *Engine) releaseSmartPauseLocked(s *smartBreaker) {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	changed := false
	for scope, st := range e.paused {
		if st.Source == PauseSourceBreaker && st.Owner == s.event.ID {
			delete(e.paused, scope)
			e.recordResumeLocked(scope)
			changed = true
		}
	}
	if changed {
		e.broadcastPauseLocked()
		e.wakeDrainNow()
	}
}

func (e *Engine) closeSmartBreakerLocked(site, stage, reason string) {
	if s := e.smartBreakers[site]; s != nil {
		if s.event != nil && s.event.Active() {
			e.transitionBreakerLocked(s, "closed", reason+"；人工操作不证明整站健康，不清空计数", time.Now())
		} else if s.event != nil {
			e.noteBreakerLocked(s, "manual_control", stage, reason+"；保留原计数和历史结论", time.Now())
		}
	}
}

func (e *Engine) admitBreakerTask(task *Task) (func(), bool) {
	e.breakerMu.Lock()
	s := e.smartBreakers[task.Site]
	if s == nil || !s.cfg.Enabled {
		e.breakerMu.Unlock()
		return func() {}, true
	}
	now := time.Now()
	if l := s.event; l != nil {
		if s.savedRevision < s.controlRevision || s.draining || (l.Active() && (!now.Before(l.Deadline) || (l.State != "observing" && l.State != "ramping"))) {
			e.breakerMu.Unlock()
			return nil, false
		}
		if l.Active() {
			if s.inflight >= s.policy.RampConcurrency || now.Before(s.nextAdmission) {
				e.breakerMu.Unlock()
				return nil, false
			}
			s.nextAdmission = now.Add(s.policy.Interval)
		}
	}
	task.breakerEpoch = s.epoch
	task.breakerTrial = s.trial
	s.inflight++
	e.breakerMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { e.breakerMu.Lock(); s.inflight--; e.breakerMu.Unlock() })
	}, true
}

func (e *Engine) recordSmartOutcome(task *Task, err error) (BreakerStatus, bool) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	s := e.smartBreakers[task.Site]
	if s == nil || !s.cfg.Enabled || task.breakerEpoch != s.epoch {
		return BreakerStatus{}, false
	}
	now := time.Now()
	f := incident.ClassifyError(err)
	sample := incident.Sample{At: now, TaskID: int64(task.ID), Stage: task.Stage, URL: task.URL, Kind: f.Category}
	if err != nil {
		sample.Error = err.Error()
	}
	sample = incident.Clean(sample)
	d := s.detector.Observe(breaker.Outcome{TaskID: int64(task.ID), Round: task.breakerRound, Site: task.Site, Stage: task.Stage, URL: sample.URL, At: now, Success: err == nil, Category: f.Category, Fingerprint: f.Fingerprint, Epoch: s.epoch, Error: sample.Error, Kind: sample.Kind})
	if !d.Accepted {
		return BreakerStatus{}, false
	}
	if s.event != nil && s.event.Active() {
		e.updateBreakerEvidenceLocked(s, d)
	}
	if err == nil {
		if s.event != nil && (s.event.State == "ramping" || s.event.State == "observing") {
			s.event.Successes++
			e.recordBreakerStageSuccessLocked(s, task, now)
		}
		return BreakerStatus{}, false
	}
	if s.event != nil && (s.event.Active() || (s.dirty && d.Rule != "")) {
		if !s.event.Active() {
			e.updateBreakerEvidenceLocked(s, d)
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
		e.noteBreakerLocked(s, "task_failed", task.Stage, "真实任务终态失败："+sample.Error, now)
		if s.event.State == "ramping" || s.event.State == "probing" {
			if d.Rule == "hard" || d.Rule == "capacity" {
				s.event.Stage, s.event.report.Rule = "", d.Rule
			}
			s.event.NextProbeAt = now.Add(s.policy.ProbeInterval * time.Duration(1<<min(s.event.ProbeAttempts, 6)))
			e.transitionBreakerLocked(s, "cooldown", "验证期间再次失败；全部阶段需重新验证", now)
			e.pauseSmartBreakerLocked(s, "限量恢复失败，重新冷却")
		} else if d.Rule == "hard" || d.Rule == "capacity" || d.Rule == "fast" || !s.event.Active() {
			s.event.Stage = ""
			s.event.report.Rule = d.Rule
			if s.event.State != "manual" && (s.event.State == "observing" || s.event.State == "ramping" || s.event.State == "probing" || !s.event.Active()) {
				e.transitionBreakerLocked(s, "cooldown", "观察期间达到快速或总数保护线", now)
				s.event.NextProbeAt = now.Add(s.policy.Cooldown)
			}
			e.pauseSmartBreakerLocked(s, "分类熔断升级保护")
		}
		return BreakerStatus{}, false
	}
	if d.Rule == "" || s.dirty {
		return BreakerStatus{}, false
	}
	e.breakerIncidentSeq++
	report := incident.Lifecycle{
		Capture: incident.Capture{ID: fmt.Sprintf("%d-%d", now.UnixNano(), e.breakerIncidentSeq), Site: task.Site, At: now, From: now.Add(-s.cfg.Window), Window: s.cfg.Window, Threshold: s.cfg.Threshold, Failures: d.Failures},
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

	l := &breakerEvent{Recovery: breaker.Recovery{ID: report.Capture.ID, Site: task.Site, State: report.State, Epoch: s.epoch, StartedAt: now, UpdatedAt: now, Deadline: report.Deadline, NextProbeAt: report.NextProbeAt, LastFailureAt: now}, report: report}
	s.event = l
	l.MixedSamples = d.Rule == "mixed"
	l.Cycle = 1
	e.resetBreakerChecksLocked(s)
	s.savedRevision = 0
	e.updateBreakerEvidenceLocked(s, d)
	state, reason := "cooldown", "同类短时爆发或窗口失败总数达到保护线"
	if d.Rule == "mixed" {
		state, reason = "observing", "混合错误短时增多，限制并发并观察"
	}
	e.transitionBreakerLocked(s, state, reason, now)
	if state == "observing" {
		return BreakerStatus{}, false
	}
	tripped := e.pauseSmartBreakerLocked(s, reason)
	st := e.breakerStatusOfLocked(task.Site)
	if st.Stage == "" {
		st.Stage = task.Stage
	}
	return st, tripped
}

func (e *Engine) updateBreakerEvidenceLocked(s *smartBreaker, d breaker.Decision) {
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
	if g := e.Governance(); g != nil {
		p := g.Effective(l.Site).Request
		l.report.AutoThrottle = p.Enabled && p.AutoThrottle.Enabled
		if until := g.RetryAfterUntil(l.Site); until.After(l.RetryAfterUntil) {
			l.RetryAfterUntil = until
		}
	}
	l.Revision++
	s.dirty = true
}

// flushBreakerReports serializes database writes, never holding the admission lock.
func (e *Engine) flushBreakerReports(ctx context.Context) error {
	e.breakerMu.Lock()
	if e.breakerStoreGate == nil {
		e.breakerStoreGate = make(chan struct{}, 1)
	}
	gate := e.breakerStoreGate
	e.breakerMu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	e.breakerMu.Lock()
	store := e.breakerStore
	var pending []breakerstore.Snapshot
	for _, s := range e.smartBreakers {
		if s.dirty && s.event != nil {
			// Window expiry can change the checkpoint without a terminal outcome.
			// A retry after a lost commit acknowledgement needs a fresh revision.
			s.event.Revision++
			pending = append(pending, s.snapshot())
		}
	}
	e.breakerMu.Unlock()
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
		e.breakerMu.Lock()
		if s := e.smartBreakers[l.Control.Site]; s != nil && s.event != nil && s.event.ID == l.Control.ID {
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
		e.breakerMu.Unlock()
	}
	return writeErr
}

// BreakerReports merges unflushed live state with durable history for the monitor.
func (e *Engine) BreakerReports(ctx context.Context, site string) ([]incident.Lifecycle, error) {
	e.breakerMu.Lock()
	store := e.breakerStore
	live := make(map[string]incident.Lifecycle)
	for key, s := range e.smartBreakers {
		if s.event != nil && (site == "" || site == key) {
			live[s.event.ID] = s.event.reportSnapshot()
		}
	}
	e.breakerMu.Unlock()
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

func (e *Engine) tickBreakers(ctx context.Context, now time.Time) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if current := time.Now(); current.After(now) {
		now = current
	}
	for site, s := range e.smartBreakers {
		l := s.event
		if l == nil {
			continue
		}
		if !l.Active() {
			if s.savedRevision >= s.controlRevision {
				e.releaseSmartPauseLocked(s)
			}
			continue
		}
		if l.State == "manual" {
			if !l.RecheckAt.IsZero() && !now.Before(l.RecheckAt) && s.savedRevision >= s.controlRevision {
				if s.probeRunning || s.probeExecuting || e.hasManualBreakerPause(site) {
					l.RecheckAt = now.Add(time.Hour)
					e.noteBreakerLocked(s, "hourly_recheck_skipped", "", "探针尚未退出或存在人工暂停；本次不启动复查，一小时后再检查", now)
				} else {
					e.beginBreakerRetryLocked(s, now, "hourly_recheck", "每小时复查：保留故障证据，重新验证全部阶段")
				}
			}
			continue
		}
		if !now.Before(l.Deadline) {
			e.pauseSmartBreakerLocked(s, "恢复超时，待人工处理")
			e.transitionBreakerLocked(s, "manual", "恢复时间预算耗尽；不能确认故障已消除", now)
			continue
		}
		if s.savedRevision < s.controlRevision {
			continue // only durable control changes can enable probes/admission
		}
		switch l.State {
		case "observing":
			if now.Sub(l.LastFailureAt) < s.policy.ObserveFor {
				continue
			}
			if e.observationRecoveredLocked(s) {
				for i := range s.event.Checks {
					s.event.Checks[i].State = "passed"
					s.event.Checks[i].FinishedAt = now
					s.event.Checks[i].Detail = "本轮观察中不同真实任务持续成功"
				}
				e.finishBreakerRecoveryLocked(s, now)
				continue
			}
			s.event.NextProbeAt = now.Add(s.policy.Cooldown)
			e.pauseSmartBreakerLocked(s, "全阶段成功证据不足，暂停整站并安排逐阶段试探")
			e.transitionBreakerLocked(s, "cooldown", "混合观察尚不能证明所有阶段正常；进入整站受控验证", now)
		case "ramping":
			e.releaseSmartPauseLocked(s)
			if now.Sub(l.UpdatedAt) >= s.policy.RampFor && e.breakerStagesReadyLocked(s, 1) {
				e.finishBreakerRecoveryLocked(s, now)
			}
		case "cooldown":
			if s.probeRunning || now.Before(l.NextProbeAt) {
				continue
			}
			if s.inflight > 0 {
				if !s.draining {
					s.draining = true
					e.noteBreakerLocked(s, "waiting_inflight", "", "等待暂停前已放行的全站任务结束，再进行一致的阶段验证", now)
				}
				continue
			}
			if l.ProbeAttempts >= s.policy.MaxProbes {
				e.transitionBreakerLocked(s, "manual", "整站试探轮数耗尽；维持暂停并等待人工处理", now)
				continue
			}
			if g := e.Governance(); g != nil {
				until := g.RetryAfterUntil(site)
				if until.After(now) {
					s.event.RetryAfterUntil, s.event.NextProbeAt = until, until
					e.noteBreakerLocked(s, "retry_after_wait", "", "遵守服务端 Retry-After，推迟整站试探", now)
					continue
				}
			}
			l.ProbeAttempts++
			e.resetBreakerChecksLocked(s)
			s.event.NextProbeAt = now
			e.transitionBreakerLocked(s, "probing", "开始一轮整站逐阶段只读语义试探；所有阶段通过前保持暂停", now)
			e.prepareNextBreakerProbeLocked(s, now)
		case "probing":
			e.launchBreakerStageProbeLocked(ctx, s, now)
		}
	}
}

func (e *Engine) observationRecoveredLocked(s *smartBreaker) bool {
	return !s.detector.Snapshot(time.Now()).Limited && e.breakerStagesReadyLocked(s, 3)
}

func (e *Engine) runBreakerProbe(ctx context.Context, probe BreakerProbe, req BreakerProbeRequest, id string, round int, timeout time.Duration) {
	defer e.tickerWG.Done()
	probeCtx, cancel := context.WithTimeout(e.RuntimeContext(ctx, req.Site), timeout)
	defer cancel()
	type outcome struct {
		result BreakerProbeResult
		err    error
	}
	ch := make(chan outcome, 1)
	// An uncooperative extension cannot stall the manager. Track its goroutine so
	// Stop never reports fully drained while user probe code is still running.
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		defer func() {
			e.breakerMu.Lock()
			if s := e.smartBreakers[req.Site]; s != nil {
				s.probeExecuting = false
			}
			e.breakerMu.Unlock()
		}()
		var o outcome
		defer func() {
			if v := recover(); v != nil {
				o.err = fmt.Errorf("probe panic: %v", v)
			}
			ch <- o
		}()
		o.result, o.err = probe(probeCtx, req)
	}()
	var o outcome
	select {
	case o = <-ch:
	case <-probeCtx.Done():
		o.err = probeCtx.Err()
	}
	o.result.Detail = incident.Clean(incident.Sample{Error: o.result.Detail}).Error
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	s := e.smartBreakers[req.Site]
	if s == nil {
		return
	}
	s.probeRunning = false
	if s.event == nil || s.event.ID != id || s.event.State != "probing" || s.event.ProbeAttempts != round {
		return
	}
	now := time.Now()
	if !now.Before(s.event.Deadline) {
		e.pauseSmartBreakerLocked(s, "恢复超时，待人工处理")
		e.transitionBreakerLocked(s, "manual", "恢复预算已耗尽，晚返回的探针不能证明按时恢复", now)
		return
	}
	if probeCtx.Err() != nil {
		o.err = probeCtx.Err()
	}
	for i := range s.event.Checks {
		check := &s.event.Checks[i]
		if check.Stage != req.Stage || check.State != "probing" {
			continue
		}
		check.FinishedAt, check.Detail = now, o.result.Detail
		complete := (check.Verification != "readonly" || (o.result.Complete && strings.TrimSpace(o.result.Detail) != "")) && (req.SampleID == "" || req.SampleID == o.result.SampleID)
		if o.err == nil && o.result.Healthy && o.result.Semantic && complete {
			check.ProbeSuccesses++
			check.State = "passed"
			if check.ProbeSuccesses < check.RequiredProbes {
				check.State = "pending"
			}
			s.event.NextProbeAt = now.Add(s.policy.Interval)
			e.noteBreakerLocked(s, "stage_probe_passed", req.Stage, fmt.Sprintf("本阶段第 %d 次语义试探通过，指定样本=%s：%s", check.ProbeSuccesses, req.SampleID, o.result.Detail), now)
			all := true
			for _, c := range s.event.Checks {
				all = all && c.State == "passed"
			}
			if all && ctx.Err() == nil {
				s.event.Successes = 0
				e.transitionBreakerLocked(s, "ramping", "所有阶段语义试探通过；进入整站限量观察，未声明完整只读覆盖的阶段仍须真实任务验证，尚不清空计数", now)
			}
			return
		}
		check.State = "failed"
		reason := "本阶段语义试探未通过，指定样本=" + req.SampleID + "：" + o.result.Detail
		if !complete {
			reason = "检查未确认完整覆盖、缺少说明或未确认指定样本；阶段仍未验证：" + o.result.Detail
		}
		if o.err != nil {
			reason += " " + o.err.Error()
		}
		check.Detail = reason
		s.event.NextProbeAt = now.Add(s.policy.ProbeInterval * time.Duration(1<<min(s.event.ProbeAttempts, 6)))
		e.noteBreakerLocked(s, "stage_probe_failed", req.Stage, reason, now)
		if ctx.Err() != nil {
			e.transitionBreakerLocked(s, "cooldown", "进程停止；保存阶段结果，重启后重新验证所有阶段", now)
		} else if errors.Is(o.err, context.DeadlineExceeded) {
			e.transitionBreakerLocked(s, "manual", "阶段探针超时；停止自动试探，保持整站暂停", now)
		} else {
			e.transitionBreakerLocked(s, "cooldown", "阶段试探失败；整站继续暂停，等待下一轮重试", now)
		}
		return
	}
}
