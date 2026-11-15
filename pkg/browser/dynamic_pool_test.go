package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitPool(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("pool state did not converge")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPoolFailedFactoryResultIsClosed(t *testing.T) {
	failed := testBrowser(true, true)
	want := errors.New("partial startup failure")
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return failed, want })
	defer p.Close()
	if _, err := p.Get(context.Background()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if !failed.closed.Load() || p.Stats().Live != 0 || p.proxyAlive.Load() != 0 {
		t.Fatalf("failed browser retained: %+v", p.Stats())
	}
}

func TestPoolNilFactoryResultReleasesReservation(t *testing.T) {
	ctx, c := governanceContext(t)
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return nil, nil })
	defer p.Close()
	if b, err := p.Get(ctx); b != nil || err == nil {
		t.Fatalf("nil factory accepted: %v %v", b, err)
	}
	if s := p.Stats(); s.Live != 0 || s.Creating != 0 || p.proxyAlive.Load() != 0 || c.Snapshot().Browser.Inflight != 0 {
		t.Fatalf("nil factory leaked admission: %+v", s)
	}
}

func TestPoolCloseDuringCreationDrainsLateResult(t *testing.T) {
	ctx, c := governanceContext(t)
	started, finish := make(chan struct{}), make(chan struct{})
	created := testBrowser(true, true)
	p := newTestPool(1, 0, func(bool) (*Browser, error) {
		close(started)
		<-finish
		return created, nil
	})
	defer p.Close()
	done := make(chan error, 1)
	go func() { _, err := p.Get(ctx); done <- err }()
	<-started
	p.Close()
	close(finish)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed pool delivered late creation")
		}
	case <-time.After(time.Second):
		t.Fatal("late creation did not settle")
	}
	if s := p.Stats(); !created.closed.Load() || s.Live != 0 || s.Creating != 0 || p.proxyAlive.Load() != 0 || c.Snapshot().Browser.Inflight != 0 {
		t.Fatalf("late creation leaked: %+v", s)
	}
}

func TestPoolPermitAfterCloseReleased(t *testing.T) {
	b := testBrowser(true, true)
	b.Close()
	released := 0
	b.setBorrowRelease(func() { released++ })
	b.releaseBorrow()
	if released != 1 {
		t.Fatalf("permit released %d times", released)
	}
}

func TestPoolResizeBeforeResourceSubscription(t *testing.T) {
	ctx, c := governanceContext(t)
	held, err := c.Acquire(ctx, "other", "browser")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	p := newTestPool(1, 1, nil)
	defer p.Close()
	// Model Get's physical reservation, followed by resize before it enters
	// acquireBrowserPermit. No policy subscription exists at this point.
	p.mu.Lock()
	p.initLocked()
	p.directAlive.Add(1)
	p.directCreating++
	p.mu.Unlock()
	if err := p.ApplyLimits(1, 0, 0); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = p.acquireBrowserPermit(bounded, false)
	p.finishCreation(false, nil)
	if !errors.Is(err, errPolicyChanged) {
		t.Fatalf("missed resize before subscription: %v", err)
	}
}
func TestPoolDynamicShrinkExpandAndDuplicateReturn(t *testing.T) {
	p := newTestPool(6, 0, nil)
	defer p.Close()
	var borrowed []*Browser
	for i := 0; i < 6; i++ {
		b, e := p.Get(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		borrowed = append(borrowed, b)
	}
	if e := p.ApplyLimits(2, 0, 0); e != nil {
		t.Fatal(e)
	}
	for _, b := range borrowed {
		if b.closed.Load() {
			t.Fatal("shrink closed borrowed instance")
		}
	}
	if !p.Stats().Converging {
		t.Fatal("missing convergence status")
	}
	if e := p.ApplyLimits(5, 0, 0); e != nil {
		t.Fatal(e)
	}
	if e := p.Put(borrowed[0]); e != nil {
		t.Fatal(e)
	}
	if !borrowed[0].closed.Load() {
		t.Fatal("excess returned instance retained")
	}
	for _, b := range borrowed[1:] {
		if e := p.Put(b); e != nil {
			t.Fatal(e)
		}
	}
	if e := p.Put(borrowed[1]); e == nil {
		t.Fatal("duplicate return accepted")
	}
	if s := p.Stats(); s.Live != 5 || s.Idle != 5 || s.Converging {
		t.Fatalf("stats: %+v", s)
	}
	if e := p.ApplyLimits(2, 1, 0); e != nil {
		t.Fatal(e)
	}
	waitPool(t, func() bool { return p.Stats().Live == 2 })
	direct, e := p.GetDirect(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	p.Put(direct)
}
func TestPoolDynamicDisableWakesWaiters(t *testing.T) {
	p := newTestPool(1, 1, nil)
	defer p.Close()
	b, _ := p.GetDirect(context.Background())
	done := make(chan error, 1)
	go func() { _, e := p.GetDirect(context.Background()); done <- e }()
	waitPool(t, func() bool { return p.Stats().Direct.Waiting == 1 })
	p.ApplyLimits(1, 0, 0)
	select {
	case e := <-done:
		if !errors.Is(e, ErrDirectDisabled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("disabled waiter stuck")
	}
	if b.closed.Load() {
		t.Fatal("disabled borrowed instance closed")
	}
	p.Put(b)
	if p.Stats().Live != 0 {
		t.Fatal("disabled pool retained return")
	}
}
func TestPoolDynamicShrinkDuringCreation(t *testing.T) {
	started, finish := make(chan struct{}, 3), make(chan struct{})
	p := newTestPool(1, 3, func(proxy bool) (*Browser, error) {
		started <- struct{}{}
		<-finish
		return testBrowser(proxy, true), nil
	})
	defer p.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := p.GetDirect(context.Background()); errs <- e }()
	}
	for i := 0; i < 3; i++ {
		<-started
	}
	if s := p.Stats(); s.Direct.Creating != 3 || s.Live != 0 {
		t.Fatalf("stats: %+v", s)
	}
	p.ApplyLimits(1, 0, 0)
	close(finish)
	wg.Wait()
	for i := 0; i < 3; i++ {
		if !errors.Is(<-errs, ErrDirectDisabled) {
			t.Fatal("late browser delivered")
		}
	}
	if s := p.Stats(); s.Live != 0 || s.Creating != 0 || p.directAlive.Load() != 0 {
		t.Fatalf("leak: %+v", s)
	}
}
func TestPoolDynamicExpansionWakesWaiterAndCancellation(t *testing.T) {
	p := newTestPool(1, 0, nil)
	defer p.Close()
	b, _ := p.Get(context.Background())
	defer p.Put(b)
	done := make(chan *Browser, 1)
	go func() { v, _ := p.Get(context.Background()); done <- v }()
	waitPool(t, func() bool { return p.Stats().Waiting == 1 })
	p.ApplyLimits(2, 0, 0)
	select {
	case v := <-done:
		if v == nil {
			t.Fatal("no expanded browser")
		}
		defer p.Put(v)
	case <-time.After(time.Second):
		t.Fatal("expansion did not wake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, e := p.Get(ctx); result <- e }()
	waitPool(t, func() bool { return p.Stats().Waiting == 1 })
	cancel()
	if !errors.Is(<-result, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestPoolDynamicIdleTimerRescheduled(t *testing.T) {
	p := newTestPool(1, 0, nil)
	defer p.Close()
	p.SetMaxIdleTime(time.Hour)
	b, _ := p.Get(context.Background())
	p.Put(b)
	p.ApplyLimits(1, 0, 10*time.Millisecond)
	waitPool(t, func() bool { return p.Stats().Live == 0 })
	p.ApplyLimits(1, 0, 0)
	b, _ = p.Get(context.Background())
	p.Put(b)
	time.Sleep(1100 * time.Millisecond)
	if b.closed.Load() {
		t.Fatal("zero idle timeout still reaped")
	}
	p.Close()
	if p.ApplyLimits(1, 0, time.Second) == nil {
		t.Fatal("closed pool accepted update")
	}
}
func TestPoolDynamicDisableCancelsResourceWait(t *testing.T) {
	ctx, c := governanceContext(t)
	held, e := c.Acquire(ctx, "other", "browser")
	if e != nil {
		t.Fatal(e)
	}
	defer held()
	p := newTestPool(1, 1, nil)
	defer p.Close()
	done := make(chan error, 1)
	go func() { _, e := p.GetDirect(ctx); done <- e }()
	waitPool(t, func() bool { return c.Snapshot().Browser.Waiting == 1 })
	p.ApplyLimits(1, 0, 0)
	select {
	case e := <-done:
		if !errors.Is(e, ErrDirectDisabled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("resource waiter not cancelled")
	}
	if p.directAlive.Load() != 0 {
		t.Fatal("creation reservation leaked")
	}
}

func TestPoolDynamicConcurrentBorrowResize(t *testing.T) {
	p := newTestPool(4, 2, nil)
	defer p.Close()
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 80; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				b, e := p.Get(ctx)
				cancel()
				if e != nil {
					t.Error(e)
					return
				}
				if e = p.Put(b); e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	for i := 0; i < 80; i++ {
		if e := p.ApplyLimits(1+i%5, i%3, 0); e != nil {
			t.Fatal(e)
		}
	}
	wg.Wait()
	p.ApplyLimits(1, 0, 0)
	waitPool(t, func() bool { return !p.Stats().Converging })
	if s := p.Stats(); s.Borrowed != 0 || s.Creating != 0 || s.Waiting != 0 || s.Live > 1 {
		t.Fatalf("leaked state: %+v", s)
	}
}
