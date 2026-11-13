package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

type blockedRestoreRepository struct {
	memoryBreakerRepository
	entered chan struct{}
	release chan struct{}
}

func (r *blockedRestoreRepository) LoadRuntime(ctx context.Context) ([]breakerstore.Snapshot, error) {
	close(r.entered)
	<-r.release // A store can still be unwinding after cancellation.
	return r.memoryBreakerRepository.LoadRuntime(ctx)
}

func TestBreakerStartupStorageIsIncludedInStopDrain(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.loggerSet, e.pools = testLoggerSet(), newPoolRegistry()
	repo := &blockedRestoreRepository{entered: make(chan struct{}), release: make(chan struct{})}
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	finished := make(chan error, 1)
	t.Cleanup(func() { cleanupBreakerStartup(t, e, repo.release, finished) })
	go func() { finished <- e.InitBreakerRuntime(context.Background()) }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("startup storage did not enter")
	}
	if drained, _ := e.Stop(20 * time.Millisecond); drained {
		t.Fatal("Stop reported drained while breaker startup storage was still active")
	}
}

func TestBreakerStartupAfterStopDoesNotAccessStorage(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	e.loggerSet, e.pools = testLoggerSet(), newPoolRegistry()
	repo := &blockedRestoreRepository{entered: make(chan struct{}), release: make(chan struct{})}
	close(repo.release)
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("empty engine failed to stop")
	}
	if err := e.InitBreakerRuntime(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("initialization after Stop returned %v, want canceled", err)
	}
	select {
	case <-repo.entered:
		t.Fatal("initialization accessed storage after Stop allowed its owner to close it")
	default:
	}
}

type blockedStartupSaveRepository struct {
	*memoryBreakerRepository
	entered chan struct{}
	release chan struct{}
}

func (r *blockedStartupSaveRepository) Save(ctx context.Context, snapshot breakerstore.Snapshot) error {
	close(r.entered)
	<-r.release
	return ctx.Err()
}

func TestBreakerStartupObserverStopRetainsInFlightStorage(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	first, store := newSmartBreakerFixture(t, 3)
	for id := 1; id <= 3; id++ {
		smartFailure(first, id, "detail", "fixture")
	}
	flushSmart(t, first)
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.loggerSet, e.pools = testLoggerSet(), newPoolRegistry()
	repo := &blockedStartupSaveRepository{memoryBreakerRepository: store, entered: make(chan struct{}), release: make(chan struct{})}
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	finished := make(chan error, 1)
	t.Cleanup(func() { cleanupBreakerStartup(t, e, repo.release, finished) })
	stopped := make(chan bool, 1)
	observed := false // The isolated consumer invokes this callback serially.
	e.SetEventsEmitter(stagePoolEventFunc(func(ev events.Event) {
		if ev.Type != events.TypeBreakerTransition || observed {
			return
		}
		observed = true
		select {
		case <-repo.entered:
		case <-repo.release:
			return
		case <-e.ctx.Done():
			return
		}
		drained, _ := e.Stop(20 * time.Millisecond)
		stopped <- drained
	}))
	go func() { finished <- e.InitBreakerRuntime(context.Background()) }()
	select {
	case drained := <-stopped:
		if drained {
			t.Fatal("restoration observer closed engine lifetime before storage returned")
		}
	case <-time.After(time.Second):
		t.Fatal("restoration observer Stop blocked on startup storage or its own callback")
	}
}

// Register before starting initialization so every assertion failure releases
// the fake store. Subprocess isolation additionally bounds a lock regression.
func cleanupBreakerStartup(t *testing.T, e *Engine, release chan struct{}, finished <-chan error) {
	t.Helper()
	e.cancel()
	close(release)
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("interrupted restoration returned %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Error("startup did not exit after store release and cancellation")
		return
	}
	e.breakers.breakerMu.Lock()
	ready := e.breakers.breakerRuntime
	e.breakers.breakerMu.Unlock()
	if ready {
		t.Error("interrupted restoration published runtime readiness")
	}
	drained := make(chan struct{})
	go func() { e.tickerWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Error("startup maintenance did not drain")
	}
	drainTestEvents(t, e)
}

type concurrentRestoreRepository struct {
	memoryBreakerRepository
	loads   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (r *concurrentRestoreRepository) LoadRuntime(ctx context.Context) ([]breakerstore.Snapshot, error) {
	if r.loads.Add(1) == 1 {
		close(r.entered)
	}
	<-r.release
	return r.memoryBreakerRepository.LoadRuntime(ctx)
}

func TestBreakerConcurrentStartupWaitIsCancelable(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	repo := &concurrentRestoreRepository{entered: make(chan struct{}), release: make(chan struct{})}
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	first := make(chan error, 1)
	second := make(chan error, 1)
	secondStarted := false
	t.Cleanup(func() {
		cleanupBreakerStartup(t, e, repo.release, first)
		if secondStarted {
			select {
			case <-second:
			case <-time.After(time.Second):
				t.Error("second initializer did not drain after cancellation")
			}
		}
	})
	go func() { first <- e.InitBreakerRuntime(context.Background()) }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("first initializer did not enter storage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	secondStarted = true
	go func() { second <- e.InitBreakerRuntime(ctx) }()
	select {
	case err := <-second:
		secondStarted = false
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting initializer returned %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent initializer entered storage instead of waiting with cancellation")
	}
	if got := repo.loads.Load(); got != 1 {
		t.Fatalf("concurrent initialization loaded runtime %d times, want 1", got)
	}
}

func TestBreakerConcurrentStartupWaitsForDurableFlush(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	first, store := newSmartBreakerFixture(t, 3)
	for id := 1; id <= 3; id++ {
		smartFailure(first, id, "detail", "fixture")
	}
	flushSmart(t, first)
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	repo := &blockedStartupSaveRepository{memoryBreakerRepository: store, entered: make(chan struct{}), release: make(chan struct{})}
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	finished := make(chan error, 1)
	t.Cleanup(func() { cleanupBreakerStartup(t, e, repo.release, finished) })
	go func() { finished <- e.InitBreakerRuntime(context.Background()) }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not enter durable flush")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.InitBreakerRuntime(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initializer bypassed pending durable flush: got %v, want deadline exceeded", err)
	}
	e.breakers.breakerMu.Lock()
	ready := e.breakers.breakerRuntime
	e.breakers.breakerMu.Unlock()
	if ready {
		t.Fatal("runtime became ready before durable flush completed")
	}
}

type cancelableRestoreRepository struct {
	memoryBreakerRepository
	entered chan struct{}
}

func (r *cancelableRestoreRepository) LoadRuntime(ctx context.Context) ([]breakerstore.Snapshot, error) {
	close(r.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBreakerStopCancelsStartupStorageContext(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, _ := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.loggerSet, e.pools = testLoggerSet(), newPoolRegistry()
	repo := &cancelableRestoreRepository{entered: make(chan struct{})}
	e.breakers.breakerRuntime, e.breakers.breakerStore = false, repo
	finished := make(chan error, 1)
	t.Cleanup(func() {
		cleanupBreakerStartup(t, e, make(chan struct{}), finished)
	})
	go func() { finished <- e.InitBreakerRuntime(context.Background()) }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not enter storage")
	}
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("Stop did not cancel and drain cooperative startup storage")
	}
}

func TestBreakerStartupFailureReleasesRestoreGate(t *testing.T) {
	if !breakerReentryChild(t) {
		return
	}
	e, repo := newSmartBreakerFixture(t, 3)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { e.cancel(); e.tickerWG.Wait() })
	e.breakers.breakerRuntime = false
	repo.err = errors.New("synthetic restore failure")
	if err := e.InitBreakerRuntime(context.Background()); !errors.Is(err, repo.err) {
		t.Fatalf("startup error = %v, want %v", err, repo.err)
	}
	if e.breakers.breakerRuntime {
		t.Fatal("failed startup published readiness")
	}
	repo.err = nil
	if err := e.InitBreakerRuntime(context.Background()); err != nil {
		t.Fatalf("retry after startup failure: %v", err)
	}
	if !e.breakers.breakerRuntime {
		t.Fatal("successful startup did not publish readiness")
	}
}
