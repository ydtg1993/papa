package engine

import (
	"context"
	"errors"

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
	sites := e.breakers.siteKeysLocked()
	e.breakers.unlockAndEmit()
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		e.breakers.breakerMu.Lock()
		s, p, task := e.breakers.reserveFailureSettlementLocked(site)
		if s == nil {
			e.breakers.unlockAndEmit()
			continue
		}
		e.breakers.unlockAndEmit()

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
			e.breakers.confirmFailureSettlementLocked(s, p)
			e.breakers.unlockAndEmit()
			// This is reconciliation, not another execution. The durable task_failed
			// and failure_settled transitions already report the actual outcome;
			// do not emit a second task execution failure after a lost checkpoint.
			e.finishTask(task)
		} else {
			e.breakers.breakerMu.Lock()
			e.breakers.deferFailureSettlementLocked(s, p, err)
			e.breakers.unlockAndEmit()
		}
		e.breakers.breakerMu.Lock()
		e.breakers.releaseFailureSettlementLocked(s)
		e.breakers.unlockAndEmit()
	}
}

// Both the original worker and restart settlement write the saved result.
// An auxiliary cleanup failure must not replace its error or compensation policy.
func pendingFailureUpdates(p *breaker.PendingFailure) map[string]any {
	return map[string]any{"status": models.TaskStatusFailed, "error": gorm.Expr("CONCAT(COALESCE(error, ''), ?)", p.Error+"\n"),
		"compensation_version": gorm.Expr("compensation_version + 1"), "failure_kind": p.FailureKind,
		"compensation_mode": p.CompensationMode, "compensation_reason": p.CompensationReason, "next_compensation_at": taskTimePointer(p.NotBefore), "last_finished_at": p.At.UTC()}
}
