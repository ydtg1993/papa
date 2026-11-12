package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/engine/internal/execution"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// attemptPersistence only persists timing and delegates the failure journal.
// Retry responsibility is shared with terminal writes through execution.State.
type attemptPersistence struct {
	db       *gorm.DB
	state    *execution.State
	breakers *breakerCoordinator
	logger   execution.Logger
}

func (p *attemptPersistence) recordTime(ctx context.Context, task *Task, column string) error {
	if task.ID == 0 {
		return nil
	}
	version, claimed := taskRunVersionFrom(ctx)
	if !claimed {
		// Only the claimed execution protocol may mutate task timing. Direct
		// internal runAttempt calls (offline parsing/archive tests) own no round.
		return nil
	}
	var captured string
	err := p.state.Retry(ctx, task.ID, stageKeyOf(task), "record "+column, func() error {
		if captured == "" {
			if err := p.db.WithContext(ctx).Raw("SELECT DATE_FORMAT(UTC_TIMESTAMP(3), '%Y-%m-%d %H:%i:%s.%f')").Scan(&captured).Error; err != nil {
				return err
			}
		}
		q := terminalScope(p.db.WithContext(ctx), task, version)
		r := q.UpdateColumn(column, captured)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			// MySQL can return zero for an unchanged millisecond timestamp.
			var row models.CrawlerTask
			if err := q.Take(&row).Error; err != nil {
				return err
			}
		}
		return nil
	}, p.logger)
	if err != nil {
		p.state.Remember(task.ID, stageKeyOf(task))
		return errors.Join(errAttemptTimingPersistence, err)
	}
	return nil
}
