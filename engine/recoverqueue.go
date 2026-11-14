package engine

import (
	"github.com/ydtg1993/papa/v4/models"
)

// Startup reset runs before any entry or worker can claim a row. Compensation
// and retry policy remain untouched; no runtime path resets processing tasks.
func (e *Engine) preparePendingRecovery() error {
	if e.db == nil {
		return nil
	}
	q := e.db.WithContext(e.queueContext()).Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusProcessing)
	// A journal already owns a known business failure. Its terminal CAS must
	// still see processing; resetting it would strand settlement indefinitely.
	// Fence the exact round, so an unrelated later round is not held hostage by
	// stale recovery evidence. Breaker restoration precedes startup recovery.
	for _, pending := range e.breakers.pendingFailureRounds() {
		q = q.Where("NOT (id = ? AND `repeat` = ? AND reprocess = ?)", pending.TaskID, pending.Repeat, pending.Reprocess)
	}
	return q.Update("status", models.TaskStatusPending).Error
}

func (e *Engine) ProcessRecoverQueue() (int, error) {
	if err := e.queueContext().Err(); err != nil {
		return 0, err
	}
	e.wakePendingRecovery()
	return 0, nil
}

func (e *Engine) startRecoverQueue() { e.startPendingRecovery(); e.wakePendingRecovery() }
