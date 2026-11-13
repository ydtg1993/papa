package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/taskstore"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// taskRunVersion fences terminal retries from a later reprocess/polling round.
// It is read only after a successful claim, before calling business code.
type taskRunVersion = taskstore.RunVersion

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
	return e.execution.Retry(ctx, task.ID, stageKeyOf(task), operation, fn, e.loggerSet.Engine)
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
	} else {
		// 队列副本可能早于首轮初始化或下一轮重置；交给 handler 的值以已认领行准。
		task.Repeat = version.Repeat
	}
	return claimed, version, err
}

func terminalScope(db *gorm.DB, task *Task, version taskRunVersion) *gorm.DB {
	return taskstore.TerminalScope(db, task.ID, version)
}

func (e *Engine) persistTerminal(ctx context.Context, task *Task, version taskRunVersion, status models.TaskStatus, cause error) error {
	if task.ID == 0 {
		return nil // directly supplied non-persistent tasks have no durable timing row
	}
	updates := taskStatusUpdates(status, cause)
	if status == models.TaskStatusFailed && !task.breakerSampleDeadline.IsZero() {
		if err := e.retainBreakerFailure(ctx, task, version, cause); err != nil {
			return err
		}
		e.breakers.breakerMu.Lock()
		updates = e.breakers.pendingTerminalUpdatesLocked(task.Site)
		e.breakers.breakerMu.Unlock()
	}
	return e.persistTerminalUpdates(ctx, task, version, status, updates)
}

func (e *Engine) persistTerminalUpdates(ctx context.Context, task *Task, version taskRunVersion, status models.TaskStatus, updates map[string]any) error {
	err := e.retryTaskState(ctx, task, "persist terminal state", func() error {
		return taskstore.Complete(e.db.WithContext(ctx), task.ID, version, status, updates)
	})
	if err == nil {
		e.forgetUnsettled(task)
	}
	return err
}

// A cancelled state retry is not a completed task. Keep it in Stop's unfinished
// snapshot even after the worker exits; the durable row is recovered on restart.
func (e *Engine) rememberUnsettled(task *Task) { e.execution.Remember(task.ID, stageKeyOf(task)) }
func (e *Engine) forgetUnsettled(task *Task)   { e.execution.Forget(task.ID) }

// incrementExecutionRetry is deliberately one-shot. On an ambiguous write the
// row stays processing and remains visible as unresolved; restart recovery can
// revisit it. Neither an additive UPDATE nor the business handler is replayed.
func (e *Engine) incrementExecutionRetry(ctx context.Context, task *Task) error {
	if task.ID != 0 {
		if err := taskstore.IncrementRetry(e.db.WithContext(ctx), task.ID); err != nil {
			e.rememberUnsettled(task)
			return fmt.Errorf("task %d retry increment unconfirmed: %w", task.ID, err)
		}
	}
	task.Retry++
	return nil
}
