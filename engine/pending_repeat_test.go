package engine

import (
	"context"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"testing"
	"time"
)

func TestPendingCapacityWaitPreservesRepeatRound(t *testing.T) {
	f := newFakeTaskDB()
	f.row["repeat"] = int64(12)
	f.row["repeatable"] = int64(models.RepeatableYes)
	pool := workerpool.NewWorkerPool[*Task](1, 1)
	if err := pool.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	e := queueEngine(t, f, pool)
	task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com", Repeat: 12, Repeatable: true}
	done := make(chan error, 1)
	go func() { done <- e.deliverPending(e.ctx, task) }()
	deadline := time.Now().Add(time.Second)
	for pool.WaitingSubmitters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.WaitingSubmitters() != 1 {
		t.Fatal("delivery did not block")
	}
	if f.written() != "" || task.Repeat != 12 {
		t.Fatal("capacity wait consumed repeat round")
	}
	delivered := make(chan *Task, 2)
	pool.Start(e.ctx, func(_ context.Context, task *Task) error { delivered <- task; return nil })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capacity did not wake delivery")
	}
	if drained, _ := pool.Stop(time.Second); !drained {
		t.Fatal("pool did not drain")
	}
	if len(delivered) != 2 {
		t.Fatal("pending task lost or duplicated")
	}
	<-delivered
	got := <-delivered
	if got.ID != 7 || got.Repeat != 12 || f.row["repeat"] != int64(12) || f.written() != "" {
		t.Fatal("delivery changed round")
	}
}
