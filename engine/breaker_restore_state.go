package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"time"
)

// acquireRestore serializes only startup transactions. Waiting is cancellable;
// neither breakerMu nor the Engine admission lock spans storage or gate waits.
func (b *breakerCoordinator) acquireRestore(ctx context.Context) (func(), error) {
	b.breakerMu.Lock()
	if b.breakerRestoreGate == nil {
		b.breakerRestoreGate = make(chan struct{}, 1)
	}
	gate := b.breakerRestoreGate
	b.breakerMu.Unlock()
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

func (b *breakerCoordinator) restoreSiteLocked(saved breakerstore.Snapshot, now time.Time, coverage map[string]string) (pending *Task, protect bool, err error) {
	if err := saved.ValidateCurrent(); err != nil {
		return nil, false, err
	}
	s := b.smartBreakers[saved.Control.Site]
	if s == nil {
		return
	} // retain the durable report for unregistered sites
	if s.event != nil && s.event.ID != saved.Control.ID {
		return nil, false, fmt.Errorf("multiple active breaker incidents for site %q", saved.Control.Site)
	}
	copy := saved.Control
	s.event = &breakerEvent{Recovery: copy, report: saved.Lifecycle.Clean()}

	s.savedRevision = saved.Control.Revision
	s.controlRevision = saved.Control.Revision
	s.epoch = saved.Control.Epoch + 1
	s.detector.Restore(*saved.Window, s.epoch, now)
	if copy.PendingFailure != nil {
		b.noteBreakerLocked(s, "failure_settlement_restart", copy.PendingFailure.Stage, "重启接续未确认失败；保留原任务轮次和错误，只重试结算，不重跑业务", now)
		pending = &Task{ID: copy.PendingFailure.TaskID, Site: copy.Site, Stage: copy.PendingFailure.Stage}
		s.controlRevision = s.event.Revision
	}
	if !copy.Active() {
		return
	}
	if copy.HistorySequence == 0 {
		b.noteBreakerLocked(s, "history_start", "", "开始追加式历史；此前尚无历史记录", now)
	}
	// Freeze the old declaration/results in history before rebuilding control.
	for _, check := range s.event.Checks {
		b.noteBreakerLocked(s, "previous_stage_snapshot", check.Stage, "重启前最后保存的阶段验证；仅留作历史，不复用健康证明", now)
	}
	if copy.StoppedAt.IsZero() {
		b.noteBreakerLocked(s, "process_interrupted", "", "启动时发现上次运行未保存正常停止记录；确切中断时间未知，旧验证作废", now)
	} else {
		b.noteBreakerLocked(s, "process_restart", "", "正常停止后重新启动；保留此前过程，旧验证作废", now)
	}
	s.event.StoppedAt = time.Time{}
	if s.event.SampleID != "" {
		b.noteBreakerLocked(s, "sample_interrupted", s.event.SampleStage, "重启作废上次样本执行；保留原截止时间，旧结果不能补算成功", now)
		b.expireBreakerSampleLocked(s, now)
	}
	s.event.Stage = "" // current recovery always protects the whole site
	b.resetBreakerChecksLocked(s, coverage)
	if copy.State != "manual" && !breakerSampleTimedOut(s) {
		if s.event.NextProbeAt.Before(now) {
			s.event.NextProbeAt = now.Add(s.policy.Cooldown)
		}
		b.transitionBreakerLocked(s, "cooldown", "进程重启：恢复暂停后重新探测", now)
	} else {
		b.transitionBreakerLocked(s, "manual", "进程重启：保留整站保护，阶段验证需重新执行", now)
	}
	protect = true
	return
}

func (b *breakerCoordinator) prepareRestore(store breakerRepository) (breakerRepository, bool) {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if b.breakerRuntime {
		return nil, true
	}
	if b.breakerStore == nil {
		b.breakerStore = store
	}
	return b.breakerStore, false
}

func (b *breakerCoordinator) restorationPublishedLocked() { b.breakerRuntime = true }

func (b *breakerCoordinator) restorationFailedLocked() { b.breakerRuntime = false }
