package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// The existing recovery snapshot is the journal. Save the result before writing
// the terminal row; a lost SQL acknowledgement can then be confirmed after restart.
func (e *Engine) retainBreakerFailure(ctx context.Context, task *Task, version taskRunVersion, cause error) error {
	return e.breakers.retainFailure(ctx, task, version, cause, &e.execution)
}

// Called by the existing report writer. sampleDispatching owns SQL outside the
// lock, and inflight prevents overlapping the original handler's cleanup.
func (e *Engine) settleBreakerFailures(ctx context.Context) {
	e.breakers.breakerMu.Lock()
	var sites []string
	for site := range e.breakers.smartBreakers {
		sites = append(sites, site)
	}
	e.breakers.breakerMu.Unlock()
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		e.breakers.breakerMu.Lock()
		s := e.breakers.smartBreakers[site]
		if !s.cfg.Enabled || s.event == nil || s.event.PendingFailure == nil || s.inflight != 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || time.Since(s.event.PendingFailure.LastAttempt) < time.Minute {
			e.breakers.breakerMu.Unlock()
			continue
		}
		p := *s.event.PendingFailure
		s.sampleDispatching = true
		s.event.PendingFailure.Attempts++
		s.event.PendingFailure.LastAttempt = time.Now()
		e.breakers.noteBreakerLocked(s, "failure_settlement_retry", p.Stage, fmt.Sprintf("第 %d 次结算任务 %d 轮次 %d:%d；只重试落库，不重跑业务", s.event.PendingFailure.Attempts, p.TaskID, p.Repeat, p.Reprocess), time.Now())
		s.controlRevision = s.event.Revision
		task := &Task{ID: p.TaskID, Site: site, Stage: p.Stage, URL: p.URL, breakerEpoch: s.epoch, breakerRound: fmt.Sprintf("%d:%d", p.Repeat, p.Reprocess), breakerSampleDeadline: p.SampleDeadline}
		e.breakers.breakerMu.Unlock()

		err := e.breakers.flushBreakerReports(ctx)
		if err == nil {
			var row models.CrawlerTask
			err = e.db.WithContext(ctx).First(&row, p.TaskID).Error
			if err == nil {
				// Preserve identity/flow/repeat bookkeeping when releasing the
				// original task. The terminal CAS still fences the saved round.
				restored := e.taskFromRecord(&row)
				restored.breakerEpoch, restored.breakerRound, restored.breakerSampleDeadline = task.breakerEpoch, task.breakerRound, p.SampleDeadline
				task = restored
			}
		}
		if err == nil {
			updates := pendingFailureUpdates(&p)
			err = e.persistTerminalUpdates(ctx, task, taskRunVersion{Repeat: p.Repeat, Reprocess: p.Reprocess}, models.TaskStatusFailed, updates)
		}
		if err == nil {
			e.recordSmartOutcome(task, errors.New(p.Error))
			e.breakers.breakerMu.Lock()
			if s.event.SampleDeadline.Equal(p.SampleDeadline) {
				e.breakers.noteBreakerLocked(s, "sample_exited", p.Stage, "原样本已退出且失败结算完成；不以结算代替健康证明", time.Now())
				s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
				s.controlRevision = s.event.Revision
			}
			e.breakers.breakerMu.Unlock()
			// This is reconciliation, not another execution. The durable task_failed
			// and failure_settled transitions already report the actual outcome;
			// do not emit a second task execution failure after a lost checkpoint.
			e.finishTask(task)
		} else {
			e.breakers.breakerMu.Lock()
			e.breakers.noteBreakerLocked(s, "failure_settlement_wait", p.Stage, "失败落库仍未确认，继续阻止验证；一分钟后重试："+err.Error(), time.Now())
			s.controlRevision = s.event.Revision
			e.breakers.breakerMu.Unlock()
		}
		e.breakers.breakerMu.Lock()
		s.sampleDispatching = false
		e.breakers.breakerMu.Unlock()
	}
}

// Both the original worker and restart settlement write the saved result.
// An auxiliary cleanup failure must not replace its error or compensation policy.
func pendingFailureUpdates(p *breaker.PendingFailure) map[string]any {
	return map[string]any{"status": models.TaskStatusFailed, "error": gorm.Expr("CONCAT(COALESCE(error, ''), ?)", p.Error+"\n"),
		"compensation_version": gorm.Expr("compensation_version + 1"), "failure_kind": p.FailureKind,
		"compensation_mode": p.CompensationMode, "compensation_reason": p.CompensationReason, "next_compensation_at": taskTimePointer(p.NotBefore), "last_finished_at": p.At.UTC()}
}
