package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/engine/internal/delivery"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"time"
)

var errDeliveryPaused = errors.New("pending delivery paused")

// retainPausedTask keeps process-local breaker evidence until the pause lifts;
// ordinary pending payloads can be reconstructed by the durable dispatcher.
func (e *Engine) retainPausedTask(t *Task) {
	if t.breakerEpoch != 0 || t.breakerTrial != 0 || !t.breakerSampleDeadline.IsZero() {
		e.deferPending(t)
		return
	}
	e.residents.release(t, "")
}

func (e *Engine) deliveryState() *delivery.Coordinator {
	e.deliveryOnce.Do(func() { e.delivery = delivery.New() })
	return e.delivery
}
func deliveryIdentity(t *Task) string {
	if t.deliveryVersion != nil {
		return fmt.Sprintf("%d/%d/%d", t.ID, t.deliveryVersion.Repeat, t.deliveryVersion.Reprocess)
	}
	return fmt.Sprintf("%d/%s", t.ID, t.Unique())
}
func (e *Engine) requestPendingRecovery(t *Task) { e.deliveryState().Wake(stageKeyOf(t)) }

// deferPending releases ordinary durable payloads. Probe deadlines remain owned
// by the existing delayed scheduler until their breaker owner settles them.
func (e *Engine) deferPending(t *Task) {
	if t.breakerEpoch != 0 || t.breakerTrial != 0 || !t.breakerSampleDeadline.IsZero() {
		if e.queueContext().Err() == nil {
			e.requeueDelayedAfter(&delayedTask{task: t}, time.Second)
			return
		}
	}
	e.residents.release(t, "")
	e.requestPendingRecovery(t)
}

// deliverPending is only called by a bounded scheduler, never a business
// handler. A full pool waits without SQL; pause or shutdown releases admission.
func (e *Engine) deliverPending(ctx context.Context, t *Task) error {
	if !e.deliveryState().AcquireForStage(t.ID, stageKeyOf(t)) {
		return nil
	}
	handed := false
	defer func() {
		if !handed {
			e.deliveryState().Release(t.ID)
			if ctx.Err() != nil || (t.breakerEpoch == 0 && t.breakerTrial == 0 && t.breakerSampleDeadline.IsZero()) {
				e.residents.release(t, "")
			}
		}
	}()
	if !e.residents.admit(t, "delivery") {
		return ErrSchedulerCapacity
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.pauseMu.Lock()
		paused := e.isPausedLocked(pauseScope{Site: t.Site, Stage: t.Stage})
		changed := e.pauseChanged
		e.pauseMu.Unlock()
		if paused {
			return errDeliveryPaused
		}
		pool := e.getOrCreatePool(stageKeyOf(t))
		if pool == nil {
			return workerpool.ErrStopped
		}
		sendCtx, cancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-changed:
				cancel()
			case <-watchDone:
			case <-sendCtx.Done():
			}
		}()
		e.residents.move(t, "pool")
		var err error
		if t.Urgent {
			err = pool.SubmitUrgentContext(sendCtx, t)
		} else {
			err = pool.SubmitContext(sendCtx, t)
		}
		close(watchDone)
		cancel()
		if err == nil {
			handed = true
			e.recoveredCount.Add(1)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.pools.IsStopped() {
			return workerpool.ErrStopped
		}
		// StopIfIdle prevents reclamation while a sender waits. A completed idle
		// retirement can still race lookup; discard only that exact stopped pool.
		if err == workerpool.ErrStopped {
			e.pools.ForgetStopped(stageKeyOf(t), pool)
		}
	}
}

func (e *Engine) startPendingRecovery() {
	e.deliveryStart.Do(func() {
		for _, key := range e.stageKeys() {
			key := key
			wake := e.deliveryState().Notifications(key)
			e.tickerWG.Add(1)
			go func() {
				defer e.tickerWG.Done()
				var timer *time.Timer
				var due <-chan time.Time
				defer func() {
					if timer != nil {
						timer.Stop()
					}
				}()
				for {
					select {
					case <-e.queueContext().Done():
						return
					case <-wake:
					case <-due:
						// Persisted deadlines can become due below the saved cursor.
						e.deliveryState().Restart(key)
					}
					// A capacity wake may have been queued before the previous scan
					// reached its end. Keep any future deadline timer in that case.
					if _, _, ended := e.deliveryState().Scan(key); ended {
						continue
					}
					d, err := e.recoverPendingStage(key)
					if err != nil && e.queueContext().Err() == nil {
						e.loggerSet.Engine.Warnf("pending recovery %s: %v", key, err)
						d = time.Second
					}
					if timer != nil {
						timer.Stop()
						timer = nil
					}
					due = nil
					if d > 0 {
						timer = time.NewTimer(d)
						due = timer.C
					}
				}
			}()
		}
	})
}

// recoverPendingStage scans once with a bounded keyset batch. Returning a delay
// schedules only a persisted deadline or storage retry, not periodic refill.
func (e *Engine) recoverPendingStage(key string) (time.Duration, error) {
	info := e.stages[key]
	if info == nil || e.db == nil {
		return 0, nil
	}
	scope := pauseScope{Site: info.config.Site, Stage: info.stage}
	cursor, revision, end := e.deliveryState().Scan(key)
	if end {
		return 0, nil
	}
	batch := min(e.schedulerLimits().RefillBatch, 1000)
	if e.cfg.RecoverQueue.BatchSize > 0 {
		batch = min(batch, e.cfg.RecoverQueue.BatchSize)
	}
	for {
		if e.queueContext().Err() != nil || e.isPaused(scope) || e.breakers.breakerSettlementBlocked(scope.Site) {
			return 0, nil
		}
		if !e.residents.available(scope.Site, key) {
			return 0, nil
		}
		var rows []models.CrawlerTask
		if err := taskstore.PendingWindow(e.queueContext(), e.db, scope.Site, scope.Stage, cursor, 0, batch).Find(&rows).Error; err != nil {
			return 0, err
		}
		for i := range rows {
			r := &rows[i]
			if r.Status != models.TaskStatusPending || r.Site != scope.Site || r.Stage != scope.Stage {
				cursor = r.ID
				if !e.deliveryState().Advance(key, revision, cursor, false) {
					return 0, nil
				}
				continue
			}
			if e.deliveryState().SkipOwned(int(r.ID), key) || e.residents.containsID(int(r.ID)) {
				cursor = r.ID
				if !e.deliveryState().Advance(key, revision, cursor, false) {
					return 0, nil
				}
				continue
			}
			t := e.taskFromRecord(r)
			if err := e.deliverPending(e.queueContext(), t); err != nil {
				if err == ErrSchedulerCapacity || err == errDeliveryPaused {
					return 0, nil
				}
				return 0, err
			}
			cursor = r.ID
			if !e.deliveryState().Advance(key, revision, cursor, false) {
				return 0, nil
			}
		}
		if len(rows) < batch {
			break
		}
	}
	delay, err := taskstore.NextPendingDelay(e.queueContext(), e.db, scope.Site, scope.Stage)
	if err == nil {
		e.deliveryState().Advance(key, revision, cursor, true)
	}
	return delay, err
}
