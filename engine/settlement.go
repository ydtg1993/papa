package engine

import (
	"context"
	"github.com/ydtg1993/papa/v3/internal/taskstore"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
	"time"
)

// settleTask retains execution ownership until the terminal write succeeds.
// Conditional processing makes retries idempotent even after an ambiguous commit.
func (e *Engine) settleTask(ctx context.Context, task *Task, status models.TaskStatus, cause error) bool {
	for {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		err := taskstore.Settle(e.db, writeCtx, uint(task.ID), status, cause, e.cfg.RepeatQueue.EffectiveInterval())
		cancel()
		if err == nil {
			return true
		}
		e.loggerSet.Engine.Errorf("settle task %d: %v", task.ID, err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// reconcileClaim is called only after an ambiguous claim write, while the task ID
// is exclusively owned by this process. No other local worker may claim this ID.
func (e *Engine) reconcileClaim(task *Task) (bool, error) {
	var row models.CrawlerTask
	err := e.db.WithContext(e.lifecycleContext()).Select("id", "status").First(&row, task.ID).Error
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Status == models.TaskStatusProcessing {
		return true, nil
	}
	if row.Status != models.TaskStatusPending {
		return false, nil
	}
	return e.claimTask(task)
}
