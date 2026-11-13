package scheduler

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

type delayed[T any] struct {
	at    time.Time
	value T
	key   string
}
type delayHeap[T any] []delayed[T]

func (h delayHeap[T]) Len() int           { return len(h) }
func (h delayHeap[T]) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h delayHeap[T]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *delayHeap[T]) Push(x any)        { *h = append(*h, x.(delayed[T])) }
func (h *delayHeap[T]) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	var zero delayed[T]
	old[n-1] = zero
	*h = old[:n-1]
	return x
}

// Delay owns the heap, wakeup channel and timer protocol. Its zero value is usable.
// Next has one dispatcher consumer; Push may run concurrently. Cancellation
// leaves queued values intact for shutdown/restart accounting by the owner.
type Delay[T any] struct {
	mu   sync.Mutex
	heap delayHeap[T]
	wake chan struct{}
	keys map[string]struct{}
}

func (q *Delay[T]) signalLocked() chan struct{} {
	if q.wake == nil {
		q.wake = make(chan struct{}, 1)
	}
	return q.wake
}
func (q *Delay[T]) Wake() <-chan struct{} { q.mu.Lock(); defer q.mu.Unlock(); return q.signalLocked() }
func (q *Delay[T]) Push(at time.Time, value T) {
	q.mu.Lock()
	heap.Push(&q.heap, delayed[T]{at: at, value: value})
	ch := q.signalLocked()
	q.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (q *Delay[T]) Len() int { q.mu.Lock(); defer q.mu.Unlock(); return q.heap.Len() }

// Snapshot returns a detached slice of queued values for shutdown accounting.
// Values are shallow copies, in heap order; it neither consumes work nor invokes
// callbacks while holding the queue lock. Mutable pointed-to values remain owned
// by the caller and should only be inspected after its dispatcher has drained.
func (q *Delay[T]) Snapshot() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]T, len(q.heap))
	for i, item := range q.heap {
		out[i] = item.value
	}
	return out
}

func (q *Delay[T]) Earliest() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.heap) == 0 {
		return time.Time{}
	}
	return q.heap[0].at
}
func (q *Delay[T]) Next(ctx context.Context) (T, bool) {
	var zero T
	for {
		if ctx.Err() != nil {
			return zero, false
		}
		q.mu.Lock()
		ch := q.signalLocked()
		if len(q.heap) == 0 {
			q.mu.Unlock()
			select {
			case <-ctx.Done():
				return zero, false
			case <-ch:
			}
			continue
		}
		wait := time.Until(q.heap[0].at)
		if wait <= 0 {
			v := heap.Pop(&q.heap).(delayed[T])
			delete(q.keys, v.key)
			q.mu.Unlock()
			return v.value, true
		}
		q.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, false
		case <-ch:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// PushUnique keeps at most one queued entry for an identity/round. The caller
// retains ownership if false is returned. Detached entries are no longer keyed.
func (q *Delay[T]) PushUnique(key string, at time.Time, value T) bool {
	q.mu.Lock()
	if q.keys == nil {
		q.keys = map[string]struct{}{}
	}
	if _, ok := q.keys[key]; ok {
		q.mu.Unlock()
		return false
	}
	q.keys[key] = struct{}{}
	heap.Push(&q.heap, delayed[T]{at: at, value: value, key: key})
	ch := q.signalLocked()
	q.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}

// ExtractMatching removes at most limit queued values. match must be a pure,
// nonblocking predicate (no scheduler/engine locks); values are returned for
// ownership transfer after the heap lock has been released.
func (q *Delay[T]) ExtractMatching(limit int, match func(T) bool) []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []T
	for i := 0; i < len(q.heap) && len(out) < limit; {
		if !match(q.heap[i].value) {
			i++
			continue
		}
		item := heap.Remove(&q.heap, i).(delayed[T])
		delete(q.keys, item.key)
		out = append(out, item.value)
	}
	return out
}
