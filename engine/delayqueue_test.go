package engine

import (
	"container/heap"
	"testing"
	"time"
)

func TestDelayHeapOrdering(t *testing.T) {
	now := time.Now()
	h := &delayHeap{}
	heap.Init(h)
	heap.Push(h, &delayedTask{at: now.Add(10 * time.Second)})
	heap.Push(h, &delayedTask{at: now.Add(2 * time.Second)})
	heap.Push(h, &delayedTask{at: now.Add(5 * time.Second)})

	want := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	for i, w := range want {
		got := heap.Pop(h).(*delayedTask).at
		if !got.Equal(now.Add(w)) {
			t.Fatalf("pop %d = %v, want %v", i, got.Sub(now), w)
		}
	}
	if h.Len() != 0 {
		t.Fatalf("heap should be empty, len=%d", h.Len())
	}
}
