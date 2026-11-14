package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v3/internal/taskstore"
	"time"

	"github.com/ydtg1993/papa/v3/internal/delivery"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
)

func (e *Engine) lifecycleContext() context.Context {
	if e.ctx == nil {
		return context.Background()
	}
	return e.ctx
}
func (e *Engine) deliveryState() *delivery.Coordinator {
	e.deliveryOnce.Do(func() { e.delivery = delivery.New() })
	return e.delivery
}

// requestPendingRecovery records one notification per stage, never a list of tasks.
func (e *Engine) requestPendingRecovery(task *Task) { e.deliveryState().Wake(stageKeyOf(task)) }
func (e *Engine) wakePendingRecovery() {
	for key := range e.stages {
		e.deliveryState().Wake(key)
	}
}

// TaskInterval returns the effective stage delay, including its configured jitter.
func (e *Engine) TaskInterval(task *Task) time.Duration {
	if s := e.stages[stageKeyOf(task)]; s != nil {
		return s.config.Delay.Random()
	}
	return 0
}

// WaitTaskReady allows a long-running handler to observe pause between pages.
func (e *Engine) WaitTaskReady(ctx context.Context, task *Task) error {
	ctx, done := delivery.Merge(ctx, e.lifecycleContext())
	defer done()
	for {
		e.pauseMu.Lock()
		paused := e.isPausedLocked(pauseScope{Site: task.Site, Stage: task.Stage})
		ch := e.pauseChanged
		e.pauseMu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// deliverPending transfers a durable task to its bounded pool. Cancellation hands
// ownership back to the durable recovery path; it never means task deletion.
func (e *Engine) deliverPending(caller context.Context, task *Task) error {
	state := e.deliveryState()
	if !state.AcquireForStage(task.ID, stageKeyOf(task)) {
		return nil
	}
	handed := false
	defer func() {
		if !handed {
			state.Release(task.ID)
			state.Wake(stageKeyOf(task))
		}
	}()
	ctx, done := delivery.Merge(caller, e.lifecycleContext())
	defer done()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.pauseMu.Lock()
		paused := e.isPausedLocked(pauseScope{Site: task.Site, Stage: task.Stage})
		if e.admissionCtx == nil {
			e.admissionCtx, e.admissionCancel = context.WithCancel(e.lifecycleContext())
		}
		changed := e.admissionCtx
		e.pauseMu.Unlock()
		if paused {
			return nil
		}
		pool := e.getOrCreatePool(stageKeyOf(task))
		if pool == nil {
			return workerpool.ErrStopped
		}
		sendCtx, sendDone := delivery.Merge(ctx, changed)
		var err error
		if task.Urgent {
			err = pool.SubmitUrgentContext(sendCtx, task)
		} else {
			err = pool.SubmitContext(sendCtx, task)
		}
		sendDone()
		if err == nil {
			handed = true
			return nil
		}
		if errors.Is(err, workerpool.ErrStopped) && !e.pools.isStopped() {
			e.pools.forgetStopped(stageKeyOf(task), pool)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A pause notification can concern a different stage; recheck the scope.
		if changed.Err() != nil {
			continue
		}
		return err
	}
}

// startPendingRecovery creates a fixed number of stage workers. They sleep on
// events; only a DB failure schedules a backoff. A full channel blocks a scan.
func (e *Engine) startPendingRecovery() {
	e.deliveryStart.Do(func() {
		for key := range e.stages {
			key := key
			wake := e.deliveryState().Notifications(key)
			e.tickerWG.Add(1)
			go func() {
				defer e.tickerWG.Done()
				for {
					select {
					case <-e.lifecycleContext().Done():
						return
					case <-wake:
					}
					for {
						if e.lifecycleContext().Err() != nil {
							return
						}
						err := e.recoverPendingStage(key)
						if err == nil {
							break
						}
						e.loggerSet.Engine.Warnf("pending delivery recovery %s: %v", key, err)
						select {
						case <-e.lifecycleContext().Done():
							return
						case <-time.After(time.Second):
						}
					}
				}
			}()
		}
	})
}

func (e *Engine) recoverPendingStage(key string) error {
	info := e.stages[key]
	if info == nil || e.db == nil {
		return nil
	}
	scope := pauseScope{Site: info.config.Site, Stage: info.stage}
	batchSize := 100
	if e.cfg != nil && e.cfg.RecoverQueue.BatchSize > 0 {
		batchSize = min(e.cfg.RecoverQueue.BatchSize, 1000)
	}
	var cursor uint
	for {
		if e.isPaused(scope) || e.lifecycleContext().Err() != nil {
			return nil
		}
		rows, err := taskstore.PendingBatch(e.db, e.lifecycleContext(), scope.Site, scope.Stage, cursor, batchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for i := range rows {
			row := &rows[i]
			cursor = row.ID
			if row.Status != models.TaskStatusPending || row.Site != scope.Site || row.Stage != scope.Stage || e.deliveryState().SkipOwned(int(row.ID), key) {
				continue
			}
			if e.isPaused(scope) {
				return nil
			}
			task := e.taskFromRecord(row)
			if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
				e.enqueueDelayed(at, task, *row)
				continue
			}
			if err := e.deliverPending(e.lifecycleContext(), task); err != nil {
				return err
			}
		}
		if len(rows) < batchSize {
			return nil
		}
	}
}
