package delivery

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorHasOneOwnerAndCanTransfer(t *testing.T) {
	c := New()
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if c.Acquire(7) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 || !c.Owned(7) {
		t.Fatal("task must have exactly one owner")
	}
	c.Release(7)
	if c.Owned(7) || !c.Acquire(7) {
		t.Fatal("released task must be available to recovery")
	}
	if !c.Acquire(8) {
		t.Fatal("unrelated task must be independent")
	}
}

func TestCoordinatorCloseRejectsNewWorkAndWaitsExistingCalls(t *testing.T) {
	c := New()
	if !c.Begin() || !c.Begin() {
		t.Fatal("open coordinator rejected work")
	}
	done := c.Close()
	if c.Begin() || c.Acquire(9) {
		t.Fatal("closed coordinator accepted new work")
	}
	c.End()
	select {
	case <-done:
		t.Fatal("returned before second call ended")
	default:
	}
	c.End()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not complete")
	}
	if c.Close() != done {
		t.Fatal("close must be idempotent")
	}
}

func TestCoordinatorStageWakeCoalescesAndSeparatesStages(t *testing.T) {
	c := New()
	s := c.Notifications("site:detail")
	for i := 0; i < 100; i++ {
		c.Wake("site:detail")
	}
	if c.Notifications("site:detail") != s || len(s) != 1 {
		t.Fatal("notifications must coalesce")
	}
	if len(c.Notifications("other:detail")) != 0 {
		t.Fatal("wake leaked across sites")
	}
	<-s
	c.Wake("site:detail")
	if len(s) != 1 {
		t.Fatal("consumed notification must be rearmable")
	}
}

func TestMergeCancelsOnEitherOwner(t *testing.T) {
	for _, engineStops := range []bool{false, true} {
		caller, cancelCaller := context.WithCancel(context.Background())
		engine, cancelEngine := context.WithCancel(context.Background())
		ctx, done := Merge(caller, engine)
		if engineStops {
			cancelEngine()
		} else {
			cancelCaller()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("merged context did not cancel")
		}
		done()
		cancelCaller()
		cancelEngine()
	}
}

func TestSkippedOwnerRearmsRecoveryOnRelease(t *testing.T) {
	for _, admission := range []bool{false, true} {
		c := New()
		c.Acquire(7)
		wake := c.Notifications("s:detail")
		if admission {
			if c.AcquireForStage(7, "s:detail") {
				t.Fatal("duplicate owner")
			}
		} else if !c.SkipOwned(7, "s:detail") {
			t.Fatal("missing owner")
		}
		if len(wake) != 0 {
			t.Fatal("must wait for owner release")
		}
		c.Release(7)
		if len(wake) != 1 {
			t.Fatal("skipped pending row stranded after release")
		}
		<-wake
		c.Acquire(7)
		c.Release(7)
		if len(wake) != 0 {
			t.Fatal("ordinary release must not schedule a scan")
		}
	}
}
