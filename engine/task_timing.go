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
	handlerErr       error
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
	return (&attemptPersistence{db: e.db, state: &e.execution, breakers: &e.breakers, logger: e.loggerSet.Engine}).recordTime(ctx, task, column)
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
