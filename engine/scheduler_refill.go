package engine

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
)

// refillPendingLocked owns bounded per-stage keyset cursors. It runs only after
// declaration freeze and independently of the optional startup recovery queue.
// The caller must hold refillMu, the drain sequencing lock (not Engine.mu).
func (e *Engine) refillPendingLocked(deadline time.Time) {
	if !e.schedulerStarted || e.residents == nil || e.db == nil {
		return
	}
	keys := e.stageKeys()
	if len(keys) == 0 {
		return
	}
	if e.refillCursor == nil {
		e.refillCursor = map[string]uint{}
	}
	limits := e.schedulerLimits()
	for n := 0; n < len(keys) && time.Now().Before(deadline); n++ {
		if e.queueContext().Err() != nil || e.residents.count() >= limits.Global {
			return
		}
		key := keys[e.refillStage%len(keys)]
		e.refillStage = (e.refillStage + 1) % len(keys)
		info := e.stages[key]
		if !e.residents.available(info.config.Site, key) || e.breakers.breakerSettlementBlocked(info.config.Site) {
			continue
		}
		if e.isPaused(pauseScope{Site: info.config.Site, Stage: info.stage}) {
			continue
		}
		batch := min(limits.RefillBatch, e.residents.capacity(info.config.Site, key))
		if batch == 0 {
			continue
		}
		var rows []models.CrawlerTask
		queryCtx, cancel := context.WithDeadline(e.queueContext(), deadline)
		err := taskstore.PendingWindow(queryCtx, e.db, info.config.Site, info.stage, e.refillCursor[key], limits.DelayWindow, batch).Find(&rows).Error
		cancel()
		if err != nil {
			if e.queueContext().Err() == nil {
				e.loggerSet.Engine.Warnf("durable scheduler refill: %v", err)
			}
			return
		}
		for i := range rows {
			if e.queueContext().Err() != nil {
				return
			}
			row := &rows[i]
			e.refillCursor[key] = row.ID
			if e.residents.containsID(int(row.ID)) {
				continue
			}
			task := e.taskFromRecord(row)
			task.deliveryVersion = &taskRunVersion{Repeat: row.Repeat, Reprocess: row.Reprocess}
			task.repeatCounted = true
			if !e.residents.admit(task, "refill") {
				break
			}
			if task.Repeatable && row.Repeat == 0 && row.Reprocess == 0 {
				task.repeatCounted = false
				e.submitToPool(task, *row)
			} else {
				e.submitPendingTask(task)
			}
		}
		if len(rows) < batch {
			e.refillCursor[key] = 0
		}
	}
}

// Snapshot pause decisions before taking the heap lock, preserving lock order.
func (e *Engine) offloadPausedDelays(limit int) {
	if e.residents == nil {
		return
	}
	paused := map[string]bool{}
	for key, info := range e.stages {
		paused[key] = e.isPaused(pauseScope{Site: info.config.Site, Stage: info.stage})
	}
	removed := e.delays.ExtractMatching(limit, func(item *delayedTask) bool {
		t := item.task
		return paused[stageKeyOf(t)] && t.deliveryDurable && t.ID != 0 && t.breakerEpoch == 0 && t.breakerTrial == 0 && t.breakerSampleDeadline.IsZero()
	})
	for _, item := range removed {
		e.residents.release(item.task, "delay")
	}
}
