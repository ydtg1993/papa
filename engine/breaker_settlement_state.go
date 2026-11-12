package engine

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

func (b *breakerCoordinator) reserveFailureSettlementLocked(site string) (state *smartBreaker, pending breaker.PendingFailure, task *Task) {
	s := b.smartBreakers[site]
	if !s.cfg.Enabled || s.event == nil || s.event.PendingFailure == nil || s.inflight != 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || time.Since(s.event.PendingFailure.LastAttempt) < time.Minute {
		return nil, breaker.PendingFailure{}, nil
	}
	p := *s.event.PendingFailure
	s.sampleDispatching = true
	s.event.PendingFailure.Attempts++
	s.event.PendingFailure.LastAttempt = time.Now()
	b.noteBreakerLocked(s, "failure_settlement_retry", p.Stage, fmt.Sprintf("第 %d 次结算任务 %d 轮次 %d:%d；只重试落库，不重跑业务", s.event.PendingFailure.Attempts, p.TaskID, p.Repeat, p.Reprocess), time.Now())
	s.controlRevision = s.event.Revision
	task = &Task{ID: p.TaskID, Site: site, Stage: p.Stage, URL: p.URL, breakerEpoch: s.epoch, breakerRound: fmt.Sprintf("%d:%d", p.Repeat, p.Reprocess), breakerSampleDeadline: p.SampleDeadline}
	return s, p, task
}

func (b *breakerCoordinator) confirmFailureSettlementLocked(s *smartBreaker, p breaker.PendingFailure) {
	if s.event.SampleDeadline.Equal(p.SampleDeadline) {
		b.noteBreakerLocked(s, "sample_exited", p.Stage, "原样本已退出且失败结算完成；不以结算代替健康证明", time.Now())
		s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
		s.controlRevision = s.event.Revision
	}
}

func (b *breakerCoordinator) deferFailureSettlementLocked(s *smartBreaker, p breaker.PendingFailure, err error) {
	b.noteBreakerLocked(s, "failure_settlement_wait", p.Stage, "失败落库仍未确认，继续阻止验证；一分钟后重试："+err.Error(), time.Now())
	s.controlRevision = s.event.Revision
}

func (b *breakerCoordinator) releaseFailureSettlementLocked(s *smartBreaker) {
	s.sampleDispatching = false
}

func (b *breakerCoordinator) ownsFailureSettlementLocked(task *Task) bool {
	s := b.smartBreakers[task.Site]
	return s != nil && s.event != nil && s.event.PendingFailure != nil && s.event.PendingFailure.TaskID == task.ID
}
func (b *breakerCoordinator) pendingTerminalUpdatesLocked(site string) map[string]any {
	return pendingFailureUpdates(b.smartBreakers[site].event.PendingFailure)
}
