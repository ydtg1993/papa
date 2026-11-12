package engine

import (
	"github.com/ydtg1993/papa/v4/pkg/events"
	"maps"
)

// queueEventLocked records an immutable observation in the current transaction.
// No emitter runs until state and associated pause changes are fully published.
func (b *breakerCoordinator) queueEventLocked(ev events.Event) {
	if b.events == nil {
		return
	}
	ev.Attrs = maps.Clone(ev.Attrs)
	ev.Counts = maps.Clone(ev.Counts)
	ev.Stages = maps.Clone(ev.Stages)
	ev.Queues = maps.Clone(ev.Queues)
	b.pendingEvents = append(b.pendingEvents, ev)
}

// unlockAndEmit detaches one finite transaction batch and then delivers it on
// this caller's stack, without any component lock or dispatch lock. Reentrant
// calls get their own batch; no goroutine or background event queue is created.
// Inner locks (pauseMu etc.) must have been released before calling this method.
func (b *breakerCoordinator) unlockAndEmit() {
	batch, emitter := b.pendingEvents, b.events
	b.pendingEvents = nil
	b.breakerMu.Unlock()
	if emitter != nil {
		for _, ev := range batch {
			emitter.Emit(ev)
		}
	}
}
