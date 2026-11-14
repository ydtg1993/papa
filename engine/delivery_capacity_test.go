package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
)

func TestDelayedProbeFullStageDoesNotBlockOtherStage(t *testing.T) {
	blocked := workerpool.NewWorkerPool[*Task](1, 1)
	if err := blocked.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	e := submitEngine(t, newFakeTaskDB(), blocked)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 8, Site: 8, Stage: 4}); err != nil {
		t.Fatal(err)
	}
	other := workerpool.NewWorkerPool[*Task](1, 1)
	e.stages["stub:other"] = &stageInfo{stage: "other", config: StageConfig{Site: "stub"}}
	e.pools.Set("stub:other", other)
	called := make(chan struct{}, 1)
	other.Start(e.ctx, func(_ context.Context, task *Task) error {
		called <- struct{}{}
		e.deliveryState().Release(task.ID)
		e.residents.release(task, "")
		return nil
	})
	done := make(chan struct{})
	go func() { e.delayDispatcher(); close(done) }()
	t.Cleanup(func() {
		e.cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("delayed dispatch did not cancel blocked admission")
		}
		other.Stop(time.Second)
	})
	e.enqueueDelayed(time.Now(), &Task{ID: 7, Site: "stub", Stage: "stub", breakerEpoch: 1}, delayRecord(7))
	deadline := time.Now().Add(time.Second)
	for blocked.WaitingSubmitters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first probe did not block on full pool")
		}
		time.Sleep(time.Millisecond)
	}
	e.enqueueDelayed(time.Now(), &Task{ID: 8, Site: "stub", Stage: "other", breakerEpoch: 1}, delayRecord(8))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("full stage blocked another stage's due recovery probe")
	}
}

func TestDeletedDelayReleasesCapacityToBlockedStage(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 1))
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	old := &Task{ID: 7, Site: "other", Stage: "old"}
	if !e.residents.admit(old, "delay") {
		t.Fatal("reserve delay")
	}
	const key = "stub:stub"
	wake := e.deliveryState().Notifications(key)
	_, revision, _ := e.deliveryState().Scan(key)
	// The pending dispatcher consumes its event but cannot query/admit while a
	// different stage's delayed task owns the only global resident slot.
	if _, err := e.recoverPendingStage(key); err != nil {
		t.Fatal(err)
	}
	e.deliverDelayed(&delayedTask{task: old})
	if e.SchedulerResidentCount() != 0 {
		t.Fatal("deleted delayed task retained capacity")
	}
	select {
	case <-wake:
	default:
		t.Fatal("deleted delay freed global capacity without waking blocked pending stage")
	}
	if _, got, _ := e.deliveryState().Scan(key); got != revision {
		t.Fatal("capacity release restarted durable scan instead of continuing")
	}
}

func TestResidentReleaseNotifiesOnceOutsideLock(t *testing.T) {
	var r *residentSet
	calls := 0
	r = newResidentSet(SchedulerLimits{}, func() {
		calls++
		if r.count() != 0 {
			t.Fatal("notification preceded release")
		}
	})
	task := &Task{ID: 1, Site: "s", Stage: "s"}
	r.admit(task, "delay")
	r.release(task, "running")
	r.release(task, "delay")
	r.release(task, "")
	if calls != 1 {
		t.Fatalf("release notifications=%d, want 1", calls)
	}
}
