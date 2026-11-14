package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

func TestPendingDeliveryWaitsWithoutSQLAndCancellationPreservesRow(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 1)
	e := submitEngine(t, f, pool)
	if err := pool.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- e.deliverPending(ctx, &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com"})
	}()
	deadline := time.Now().Add(time.Second)
	for pool.WaitingSubmitters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.WaitingSubmitters() != 1 {
		t.Fatal("delivery did not wait at bounded queue")
	}
	before := f.readSQL() + f.written()
	time.Sleep(30 * time.Millisecond)
	if got := f.readSQL() + f.written(); got != before || got != "" {
		t.Fatalf("capacity wait performed SQL: %s", got)
	}
	if queued(t, pool) != 1 {
		t.Fatal("capacity exceeded")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not wake delivery")
	}
	if f.row["status"] != int64(models.TaskStatusPending) || f.written() != "" {
		t.Fatal("cancel changed durable pending")
	}
	pool.Stop(0)
}

func TestPendingRecoverySkipsNonPendingAndMissingRows(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusFailed, models.TaskStatusSuccess, models.TaskStatusProcessing} {
		t.Run(string(rune('0'+status)), func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(status)
			// The fake records SQL but does not evaluate status predicates.
			// Model the database excluding this row, and verify the predicate.
			f.noRows = true
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)
			if _, err := e.recoverPendingStage("stub:stub"); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 0 || f.written() != "" {
				t.Fatal("recovery reactivated nonpending work")
			}
			if !strings.Contains(f.readSQL(), "status = ?") {
				t.Fatal("recovery scan did not constrain pending status")
			}
		})
	}
	f := newFakeTaskDB()
	f.noRows = true
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)
	if _, err := e.recoverPendingStage("stub:stub"); err != nil {
		t.Fatal(err)
	}
	if queued(t, pool) != 0 {
		t.Fatal("missing task was enqueued")
	}
}

func TestPendingRecoveryRetriesReadFailureWithoutRewritingState(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)
	if _, err := e.recoverPendingStage("stub:stub"); err == nil {
		t.Fatal("read failure hidden")
	}
	if queued(t, pool) != 0 {
		t.Fatal("unread task delivered")
	}
	if _, err := e.recoverPendingStage("stub:stub"); err != nil {
		t.Fatal(err)
	}
	if queued(t, pool) != 1 {
		t.Fatal("durable pending was not retried")
	}
	if strings.Contains(f.written(), "UPDATE") {
		t.Fatalf("recovery rewrote pending: %s", f.written())
	}
}

func TestPendingScanAtEndIgnoresCapacityUntilBusinessEvent(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 1))
	if _, err := e.recoverPendingStage("stub:stub"); err != nil {
		t.Fatal(err)
	}
	before := f.readSQL()
	for i := 0; i < 20; i++ {
		e.deliveryState().Continue("stub:stub")
		if _, err := e.recoverPendingStage("stub:stub"); err != nil {
			t.Fatal(err)
		}
	}
	if f.readSQL() != before {
		t.Fatal("capacity releases restarted exhausted pending scan")
	}
	e.requestPendingRecovery(&Task{Site: "stub", Stage: "stub"})
	if _, err := e.recoverPendingStage("stub:stub"); err != nil {
		t.Fatal(err)
	}
	if f.readSQL() == before {
		t.Fatal("business event failed to restart scan")
	}
}

func TestPausedWorkerPreservesBreakerProbeIdentityAndResidentBudget(t *testing.T) {
	f := newFakeTaskDB()
	e := newTestEngine(t, f)
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 2, Site: 2, Stage: 2}); err != nil {
		t.Fatal(err)
	}
	e.SetSiteBreaker("stub", breaker.Config{Enabled: false, Threshold: 1})
	if err := e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 1}, &recordingFetcher{stage: "stub"}, nil); err != nil {
		t.Fatal(err)
	}
	if !e.PauseSite("stub", "test pause after worker receives probe") {
		t.Fatal("pause failed")
	}
	deadline := time.Now().Add(time.Minute)
	probe := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com", breakerEpoch: 3, breakerTrial: 4, breakerSampleDeadline: deadline}
	if !e.residents.admit(probe, "pool") {
		t.Fatal("probe admission")
	}
	if err := e.stageHandler(e.stages["stub:stub"])(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	if e.delays.Len() != 1 || e.SchedulerResidentCount() != 1 {
		t.Fatalf("probe retention delays=%d residents=%d", e.delays.Len(), e.SchedulerResidentCount())
	}
	if probe.breakerEpoch != 3 || probe.breakerTrial != 4 || !probe.breakerSampleDeadline.Equal(deadline) {
		t.Fatal("paused probe identity or original deadline changed")
	}
	if f.written() != "" {
		t.Fatalf("paused probe claimed work: %s", f.written())
	}
}
