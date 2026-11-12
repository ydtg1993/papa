package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

// breakerSampleReservation carries a generation token across unlocked SQL.
// Only coordinator operations dereference its state/event pointers.
type breakerSampleReservation struct {
	state     *smartBreaker
	event     *breakerEvent
	id, stage string
	deadline  time.Time
	taskID    int64
}

func (b *breakerCoordinator) reserveSampleLocked(site string, manualPause bool) *breakerSampleReservation {
	s := b.smartBreakers[site]
	l, now := s.event, time.Now()
	if l == nil || l.PendingFailure != nil || !l.MixedSamples || (l.State != "observing" && l.State != "ramping") || !now.Before(l.Deadline) || s.draining || s.inflight > 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || now.Before(s.nextSampleRetry) || manualPause {
		return nil
	}
	b.expireBreakerSampleLocked(s, now)
	if l.SampleID == "" {
		stage, id := nextBreakerSample(s)
		if id != "" {
			b.selectBreakerSampleLocked(s, stage, id, now)
		}
		return nil
	}
	taskID, parseErr := strconv.ParseInt(strings.TrimPrefix(l.SampleID, "task:"), 10, 64)
	if parseErr != nil || taskID <= 0 || s.savedRevision < s.controlRevision {
		return nil
	}

	s.sampleDispatching = true
	s.nextSampleRetry = now.Add(max(time.Second, s.policy.Interval))

	return &breakerSampleReservation{state: s, event: l, id: l.ID, stage: l.SampleStage, deadline: l.SampleDeadline, taskID: taskID}
}

func (b *breakerCoordinator) finishSampleLocked(token *breakerSampleReservation, requeued bool, err error) {
	s, l, id, stage, deadline, taskID := token.state, token.event, token.id, token.stage, token.deadline, token.taskID
	s.sampleDispatching = false
	if s.event == l && l.ID == id && l.SampleDeadline.Equal(deadline) {
		if requeued {
			b.noteBreakerLocked(s, "sample_retry", stage, fmt.Sprintf("串行重投验证样本 %d；复用原有任务认领与版本规则", taskID), time.Now())
		} else if err != nil && !errors.Is(err, ErrTaskChanged) && !errors.Is(err, ErrTaskProcessing) {
			b.noteBreakerLocked(s, "sample_retry_unavailable", stage, fmt.Sprintf("验证样本 %d 暂不能重试：%v；等待至原截止时间，不延长预算", taskID, err), time.Now())
			s.nextSampleRetry = time.Now().Add(max(s.policy.ProbeInterval, s.policy.Interval))
		}
		b.expireBreakerSampleLocked(s, time.Now())
	}
}

func (b *breakerCoordinator) sampleValidLocked(token *breakerSampleReservation, manualPause bool) bool {
	s, l, deadline := token.state, token.event, token.deadline
	return s.event == l && l.SampleDeadline.Equal(deadline) && time.Now().Before(deadline) && (l.State == "observing" || l.State == "ramping") && !manualPause
}
