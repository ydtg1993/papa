package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestDelayDispatchBoundsEachGroupAndRetainsQueuedOnCancel(t *testing.T) {
	var q Delay[int]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 3)
	done := make(chan struct{})
	q.PushUnique("first", time.Now(), 1)
	q.PushUnique("second", time.Now(), 2)
	q.PushUnique("other", time.Now(), 3)
	go func() {
		q.Dispatch(ctx, func(v int) string {
			if v < 3 {
				return "blocked"
			}
			return "other"
		}, func(v int) {
			started <- v
			<-ctx.Done()
		})
		close(done)
	}()
	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case v := <-started:
			seen[v] = true
		case <-time.After(time.Second):
			t.Fatal("independent group did not start")
		}
	}
	if !seen[1] || !seen[3] || q.Len() != 1 {
		t.Fatalf("per-group serialization lost: started=%v queued=%d", seen, q.Len())
	}
	if q.PushUnique("second", time.Now(), 2) {
		t.Fatal("blocked group lost queued identity")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not join canceled handlers")
	}
	if values := q.Snapshot(); len(values) != 1 || values[0] != 2 {
		t.Fatalf("cancel lost queued work: %v", values)
	}
}
