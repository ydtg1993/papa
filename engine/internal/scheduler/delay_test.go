package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestDelayHeapOrdering(t *testing.T) {
	var q Delay[int]
	now := time.Now().Add(-time.Minute)
	q.Push(now.Add(10*time.Second), 10)
	q.Push(now.Add(2*time.Second), 2)
	q.Push(now.Add(5*time.Second), 5)
	for _, want := range []int{2, 5, 10} {
		got, ok := q.Next(context.Background())
		if !ok || got != want {
			t.Fatalf("got %d,%v want %d", got, ok, want)
		}
	}
	if q.Len() != 0 {
		t.Fatal("queue not empty")
	}
}
func TestDelayEarlierArrivalAndCancellation(t *testing.T) {
	var q Delay[int]
	q.Push(time.Now().Add(time.Hour), 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := make(chan int, 1)
	go func() { v, _ := q.Next(ctx); got <- v }()
	q.Push(time.Now(), 2)
	select {
	case v := <-got:
		if v != 2 {
			t.Fatalf("got %d", v)
		}
	case <-ctx.Done():
		t.Fatal("earlier entry did not wake dispatcher")
	}
	cancel()
	if _, ok := q.Next(ctx); ok {
		t.Fatal("canceled dispatcher consumed work")
	}
	if q.Len() != 1 {
		t.Fatal("cancel lost future entry")
	}
}
