package scheduler

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Dispatch owns the queue's sole consumer and runs at most one handler per
// group. A blocked group leaves its remaining values in the original heap;
// other groups can still deliver due work. group must be pure and nonblocking.
// The caller bounds group cardinality (the engine uses registered stages).
// Handlers must observe ctx; Dispatch waits for them before returning on cancel.
// Do not run concurrently with Next or another Dispatch.
func (q *Delay[T]) Dispatch(ctx context.Context, group func(T) string, handle func(T)) {
	busy := make(map[string]bool)
	finished := make(chan string)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		if ctx.Err() != nil {
			return
		}
		q.mu.Lock()
		wake := q.signalLocked()
		next := -1
		if len(q.heap) > 0 && !busy[group(q.heap[0].value)] {
			next = 0
		} else {
			for i, item := range q.heap {
				if !busy[group(item.value)] && (next < 0 || item.at.Before(q.heap[next].at)) {
					next = i
				}
			}
		}
		var timer *time.Timer
		var due <-chan time.Time
		if next >= 0 {
			wait := time.Until(q.heap[next].at)
			if wait <= 0 {
				item := heap.Remove(&q.heap, next).(delayed[T])
				delete(q.keys, item.key)
				key := group(item.value)
				busy[key] = true
				q.mu.Unlock()
				handlers.Add(1)
				go func() {
					defer handlers.Done()
					handle(item.value)
					select {
					case finished <- key:
					case <-ctx.Done():
					}
				}()
				continue
			}
			timer = time.NewTimer(wait)
			due = timer.C
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
		case key := <-finished:
			delete(busy, key)
		case <-wake:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
