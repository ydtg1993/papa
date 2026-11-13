package engine

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v4/internal/taskstore"
)

// durableStopBacklog counts pending rows without rebuilding task payloads. A
// completed lifecycle can still have intentionally deferred durable work.
func (e *Engine) durableStopBacklog(deadline time.Time, stats map[string]StopStats) bool {
	if e.residents == nil || e.db == nil {
		return true
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, key := range e.stageKeys() {
		info := e.stages[key]
		count, err := taskstore.CountPending(ctx, e.db, info.config.Site, info.stage)
		if err != nil {
			e.loggerSet.Engine.Warnf("durable pending shutdown count unconfirmed: %v", err)
			return false
		}
		if count > 0 {
			st := stats[key]
			st.Queued = max(st.Queued, int(count))
			stats[key] = st
		}
	}

	return true
}
