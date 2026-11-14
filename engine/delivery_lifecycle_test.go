package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
)

func waitDeliveryCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("delivery condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeliveryFullQueueWaitsWithoutDatabaseWrites(t *testing.T) {
	f := newFakeTaskDB()
	p := workerpool.NewWorkerPool[*Task](1, 1)
	e := submitEngine(t, f, p)
	if err := p.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- e.deliverPending(ctx, &Task{ID: 7, Site: "stub", Stage: "stub"}) }()
	waitDeliveryCondition(t, func() bool { return p.WaitingSubmitters() == 1 })
	if !e.deliveryState().Owned(7) {
		t.Fatal("blocked sender lost delivery ownership")
	}
	if f.written() != "" || f.readSQL() != "" {
		t.Fatal("queue capacity wait must not query or rewrite tasks")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release sender")
	}
	if e.deliveryState().Owned(7) || len(e.deliveryState().Notifications(stubKey)) != 1 {
		t.Fatal("cancel must transfer accepted task to durable recovery")
	}
	if f.written() != "" {
		t.Fatal("cancel must not mark the task failed")
	}
}

func TestDeliveryPauseReleasesBlockedSenderWithoutLosingTask(t *testing.T) {
	f := newFakeTaskDB()
	p := workerpool.NewWorkerPool[*Task](1, 1)
	e := submitEngine(t, f, p)
	if err := p.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- e.deliverPending(context.Background(), &Task{ID: 7, Site: "stub", Stage: "stub"}) }()
	waitDeliveryCondition(t, func() bool { return p.WaitingSubmitters() == 1 })
	e.setPause(pauseScope{Site: "stub"}, pauseState{Source: PauseSourceManual})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pause did not release blocked sender")
	}
	if e.deliveryState().Owned(7) {
		t.Fatal("paused task must remain eligible for recovery")
	}
	if f.written() != "" {
		t.Fatal("pause should not change durable pending state")
	}
	if !e.clearScope(pauseScope{Site: "stub"}) {
		t.Fatal("resume failed")
	}
	// Use a fresh bounded pool to emulate available capacity after resume.
	p2 := workerpool.NewWorkerPool[*Task](1, 1)
	e.pools.set(stubKey, p2)
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if queued(t, p2) != 1 || !e.deliveryState().Owned(7) {
		t.Fatal("resume recovery did not hand task back to the channel")
	}
	if f.written() != "" {
		t.Fatal("pending recovery must not rewrite state")
	}
}

func TestPendingRecoverySkipsOwnedTerminalAndProcessingRows(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing, models.TaskStatusSuccess, models.TaskStatusFailed} {
		t.Run(statusName(status), func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(status)
			p := workerpool.NewWorkerPool[*Task](1, 2)
			e := submitEngine(t, f, p)
			if status == models.TaskStatusPending {
				e.deliveryState().Acquire(7)
			}
			if err := e.recoverPendingStage(stubKey); err != nil {
				t.Fatal(err)
			}
			if queued(t, p) != 0 || f.written() != "" {
				t.Fatal("recovery must neither duplicate ownership nor reset active/terminal tasks")
			}
		})
	}
}

func TestPendingRecoveryMissingRowsAndTransientReadFailure(t *testing.T) {
	f := newFakeTaskDB()
	p := workerpool.NewWorkerPool[*Task](1, 2)
	e := submitEngine(t, f, p)
	f.failQueries = 1
	if err := e.recoverPendingStage(stubKey); err == nil {
		t.Fatal("DB failure must be returned for backoff")
	}
	if queued(t, p) != 0 || e.deliveryState().Owned(7) {
		t.Fatal("failed read must not claim task ownership")
	}
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if queued(t, p) != 1 {
		t.Fatal("pending task was lost after transient DB failure")
	}
	f.noRows = true
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if queued(t, p) != 1 {
		t.Fatal("missing task must not produce another delivery")
	}
}

func TestPendingRecoveryPausedStageDoesNotQuery(t *testing.T) {
	f := newFakeTaskDB()
	p := workerpool.NewWorkerPool[*Task](1, 2)
	e := submitEngine(t, f, p)
	e.setPause(pauseScope{Site: "stub"}, pauseState{})
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if f.readSQL() != "" || f.written() != "" || queued(t, p) != 0 {
		t.Fatal("paused scope must not scan or deliver")
	}
}

func TestRepeatRoundCountChangesOnlyAtDurableReset(t *testing.T) {
	f := newFakeTaskDB()
	p := workerpool.NewWorkerPool[*Task](1, 2)
	e := queueEngine(t, f, p)
	row := actionRow(7, "stub")
	row.Status = models.TaskStatusSuccess
	row.Repeatable = models.RepeatableYes
	row.Repeat = 12
	if !e.requeueRepeatTaskIn(row) {
		t.Fatal("repeat reset failed")
	}
	resetWrites := f.written()
	if strings.Count(resetWrites, "`repeat` + 1") != 1 {
		t.Fatalf("round must increment once in reset: %s", resetWrites)
	}
	// The synthetic driver does not execute UPDATE expressions; emulate commit.
	f.row["repeat"] = int64(13)
	for i := 0; i < 3; i++ {
		if err := e.recoverPendingStage(stubKey); err != nil {
			t.Fatal(err)
		}
	}
	if f.written() != resetWrites || queued(t, p) != 1 {
		t.Fatal("repeated recovery duplicated delivery or incremented round again")
	}
}
