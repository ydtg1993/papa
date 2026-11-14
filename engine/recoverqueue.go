package engine

import (
	"github.com/ydtg1993/papa/v3/internal/taskstore"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// preparePendingRecovery runs before ANY pool/entry is started. In a single
// instance process, all processing rows at this boundary belong to the old run.
func (e *Engine) preparePendingRecovery() error {
	if e.db == nil {
		return nil
	}
	return taskstore.ResetOrphans(e.db, e.lifecycleContext())
}
func (e *Engine) recoverQueueQuery() func() *gorm.DB {
	return func() *gorm.DB {
		return e.db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusPending)
	}
}

// ProcessRecoverQueue requests durable pending recovery. It never resets live
// processing rows; each stage has its own bounded delivery worker.
func (e *Engine) ProcessRecoverQueue() (int, error) {
	if e.lifecycleContext().Err() != nil {
		return 0, e.lifecycleContext().Err()
	}
	e.wakePendingRecovery()
	return 0, nil
}
func (e *Engine) requeueRecoverTask(t *models.CrawlerTask) bool {
	if e.stages[stageKeyOfRecord(t)] == nil || t.Status != models.TaskStatusPending {
		return false
	}
	if err := e.submitRecoveredTask(e.taskFromRecord(t)); err != nil {
		e.loggerSet.Engine.Warnf("recover task %d: %v", t.ID, err)
		return false
	}
	e.recoveredCount.Add(1)
	return true
}
func (e *Engine) startRecoverQueue() { e.startPendingRecovery(); e.wakePendingRecovery() }
