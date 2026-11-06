package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// taskRunVersion fences terminal retries from a later reprocess/polling round.
// It is read only after a successful claim, before calling business code.
type taskRunVersion struct {
	Repeat    int
	Reprocess int
}

type taskRunVersionKey struct{}

func withTaskRunVersion(ctx context.Context, version taskRunVersion) context.Context {
	return context.WithValue(ctx, taskRunVersionKey{}, version)
}

func taskRunVersionFrom(ctx context.Context) (taskRunVersion, bool) {
	version, ok := ctx.Value(taskRunVersionKey{}).(taskRunVersion)
	return version, ok
}

// retryTaskState retains responsibility in the current worker. It retries only
// the state operation, never the FetchHandler. Persistent outages apply bounded
// backpressure (100ms up to 2s between attempts) until cancellation.
func (e *Engine) retryTaskState(ctx context.Context, task *Task, operation string, fn func() error) error {
	delay := 100 * time.Millisecond
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			e.rememberUnsettled(task)
			return fmt.Errorf("task %d %s interrupted: %w", task.ID, operation, errors.Join(err, lastErr))
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, gorm.ErrRecordNotFound) || errors.Is(lastErr, ErrTaskChanged) {
			return fmt.Errorf("task %d %s: %w", task.ID, operation, lastErr)
		}
		if attempt == 0 || attempt%10 == 0 {
			e.loggerSet.Engine.Errorf("task %d %s failed; retaining worker and retrying: %s", task.ID, operation, lastErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		delay = min(delay*2, 2*time.Second)
	}
}

func (e *Engine) claimForExecution(ctx context.Context, task *Task) (bool, taskRunVersion, error) {
	var claimed bool
	var uncertain bool
	var version taskRunVersion
	err := e.retryTaskState(ctx, task, "claim", func() error {
		var err error
		claimed, err = e.claimTaskContext(ctx, task)
		uncertain = uncertain || err != nil
		return err
	})
	if err == nil && !claimed && uncertain {
		// An error may have lost the acknowledgement of a committed claim.
		// Without an ownership token it is unsafe to assume that processing is
		// ours. Expose the unresolved row instead of executing or calling it done.
		e.rememberUnsettled(task)
		return false, version, fmt.Errorf("task %d claim outcome unconfirmed: %w", task.ID, ErrTaskChanged)
	}
	if err != nil || !claimed || task.ID == 0 {
		return claimed, version, err
	}
	// A confirmed claim prevents normal in-process task admin/queues from
	// replacing this round while we read its version. Multi-process leases are
	// intentionally outside this single-process execution protocol.
	err = e.retryTaskState(ctx, task, "read claimed round", func() error {
		return e.db.WithContext(ctx).Model(&models.CrawlerTask{}).
			Select([]string{"repeat", "reprocess"}).Where("id = ?", task.ID).Take(&version).Error
	})
	if err != nil {
		e.releaseClaim(task)
	}
	return claimed, version, err
}

func terminalScope(db *gorm.DB, task *Task, version taskRunVersion) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ? AND `repeat` = ? AND reprocess = ?",
			task.ID, models.TaskStatusProcessing, version.Repeat, version.Reprocess)
}

func (e *Engine) persistTerminal(ctx context.Context, task *Task, version taskRunVersion, status models.TaskStatus, cause error) error {
	if task.ID == 0 {
		return nil // compatibility for non-persistent, directly supplied tasks
	}
	updates := taskStatusUpdates(status, cause)
	err := e.retryTaskState(ctx, task, "persist terminal state", func() error {
		db := e.db.WithContext(ctx)
		result := terminalScope(db, task, version).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			return nil
		}
		// A lost acknowledgement may mean the previous UPDATE committed. The
		// processing guard makes its error append idempotent. Confirm that exact
		// terminal round; never overwrite a later pending/processing generation.
		var current models.CrawlerTask
		if err := db.Select([]string{"status", "repeat", "reprocess"}).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status == status && current.Repeat == version.Repeat && current.Reprocess == version.Reprocess {
			return nil
		}
		return ErrTaskChanged
	})
	if err == nil {
		e.forgetUnsettled(task)
	}
	return err
}

// A cancelled state retry is not a completed task. Keep it in Stop's unfinished
// snapshot even after the worker exits; the durable row is recovered on restart.
func (e *Engine) rememberUnsettled(task *Task) {
	if task.ID == 0 {
		return
	}
	e.persistenceMu.Lock()
	defer e.persistenceMu.Unlock()
	if e.unsettled == nil {
		e.unsettled = make(map[int]string)
	}
	e.unsettled[task.ID] = stageKeyOf(task)
}

func (e *Engine) forgetUnsettled(task *Task) {
	e.persistenceMu.Lock()
	defer e.persistenceMu.Unlock()
	delete(e.unsettled, task.ID)
}
