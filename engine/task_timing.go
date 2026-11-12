package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/models"
	"time"
)

var errAttemptTimingPersistence = errors.New("attempt timing persistence unresolved")

type attemptTimingError struct {
	cause            error
	handlerSucceeded bool
}

func (e *attemptTimingError) Error() string { return e.cause.Error() }
func (e *attemptTimingError) Unwrap() error { return e.cause }

func taskDelayMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if d > 0 && d%time.Millisecond != 0 {
		ms++
	}
	if d < 0 && ms == 0 {
		return -1
	}
	return ms
}

func taskTimePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
func taskTimeValue(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (e *Engine) recordAttemptTime(ctx context.Context, task *Task, column string) error {
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
	err := e.retryTaskState(ctx, task, "record "+column, func() error {
		if captured == "" {
			if err := e.db.WithContext(ctx).Raw("SELECT DATE_FORMAT(UTC_TIMESTAMP(3), '%Y-%m-%d %H:%i:%s.%f')").Scan(&captured).Error; err != nil {
				return err
			}
		}
		q := terminalScope(e.db.WithContext(ctx), task, version)
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
	})
	if err != nil {
		e.rememberUnsettled(task)
		return errors.Join(errAttemptTimingPersistence, err)
	}
	return nil
}

// Defer from any delivery route using the durable deadline. Memory time is only
// a wake-up hint; claimScope checks the database clock before execution.
func (e *Engine) deferUntilDue(task *Task) bool {
	if !task.NotBefore.IsZero() && task.NotBefore.After(time.Now()) {
		e.enqueueDelayed(task.NotBefore, task, models.CrawlerTask{ID: uint(task.ID)})
		return true
	}
	return false
}
