package workerpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The first gate succeeds, simulating an idle worker that passed the gate before
// pause. The second gate must prevent the newly received task from executing.
type admissionPauseGate struct {
	calls  atomic.Int32
	first  chan struct{}
	resume chan struct{}
}

func (g *admissionPauseGate) Wait(stop <-chan struct{}) bool {
	if g.calls.Add(1) == 1 {
		close(g.first)
		return true
	}
	select {
	case <-g.resume:
		return true
	case <-stop:
		return false
	}
}

func TestPauseAfterIdleReceiveGateBlocksExecutionAndStopReleases(t *testing.T) {
	for _, stop := range []bool{false, true} {
		p := NewWorkerPool[*testTask](1, 1)
		g := &admissionPauseGate{first: make(chan struct{}), resume: make(chan struct{})}
		p.SetGate(g)
		ran := make(chan struct{}, 1)
		p.Start(context.Background(), func(context.Context, *testTask) error { ran <- struct{}{}; return nil })
		<-g.first
		if err := p.SubmitContext(context.Background(), &testTask{}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ran:
			t.Fatal("task executed after pause while idle")
		case <-time.After(30 * time.Millisecond):
		}
		if !stop {
			close(g.resume)
		}
		if drained, _ := p.Stop(time.Second); !drained {
			t.Fatal("paused received task prevented shutdown")
		}
		if len(ran) != 1 {
			t.Fatal("received task lost or duplicated")
		}
	}
}

func TestIdleStopIsAtomicWithAdmission(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := NewWorkerPool[*testTask](1, 1)
		finish := make(chan struct{})
		p.Start(context.Background(), func(context.Context, *testTask) error { <-finish; return nil })
		start := make(chan struct{})
		sent := make(chan error, 1)
		go func() { <-start; sent <- p.SubmitContext(context.Background(), &testTask{}) }()
		close(start)
		reaped := p.StopIfIdle(time.Second)
		err := <-sent
		if reaped && err == nil {
			t.Fatal("reaper detached a pool that accepted unfinished work")
		}
		if !reaped && err != nil {
			t.Fatalf("live pool rejected admission: %v", err)
		}
		close(finish)
		if drained, _ := p.Stop(time.Second); !drained {
			t.Fatal("pool failed to drain")
		}
	}
}

func TestSubmitContextWaitsForConsumerAtFullCapacity(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 2)
	for i := 0; i < 2; i++ {
		if err := p.SubmitContext(context.Background(), &testTask{url: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.SubmitContext(ctx, &testTask{url: "waiting"}) }()
	waitFor(t, func() bool { return p.WaitingSubmitters() == 1 }, "blocked admission")
	select {
	case err := <-result:
		t.Fatalf("full queue returned instead of waiting: %v", err)
	default:
	}
	ran := make(chan string, 3)
	p.Start(context.Background(), func(_ context.Context, task *testTask) error {
		ran <- task.url
		return nil
	})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if drained, remaining := p.Stop(time.Second); !drained || remaining != 0 {
		t.Fatalf("stop = %v, %d", drained, remaining)
	}
	if len(ran) != 3 {
		t.Fatalf("executed %d tasks, want 3", len(ran))
	}
}

func TestSubmitContextCancellationDoesNotAcceptTask(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 1)
	if err := p.SubmitContext(context.Background(), &testTask{url: "first"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- p.SubmitContext(ctx, &testTask{url: "cancelled"}) }()
	waitFor(t, func() bool { return p.WaitingSubmitters() == 1 }, "blocked admission")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release submitter")
	}
	if n, _, _, _, _ := p.Stats(); n != 1 {
		t.Fatalf("cancelled submission was accepted: submitted = %d", n)
	}
	if got := (<-p.taskQueue).url; got != "first" {
		t.Fatalf("queued task = %q", got)
	}
	p.Stop(time.Second)
}

func TestSubmitContextStopWakesAllWaiters(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 1)
	if err := p.SubmitContext(context.Background(), &testTask{}); err != nil {
		t.Fatal(err)
	}
	const count = 16
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() { results <- p.SubmitContext(context.Background(), &testTask{}) }()
	}
	waitFor(t, func() bool { return p.WaitingSubmitters() == count }, "all blocked submitters")
	p.Stop(time.Second)
	for i := 0; i < count; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrStopped) {
				t.Fatalf("error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Stop blocked behind a sender or did not wake it")
		}
	}
	if err := p.SubmitContext(context.Background(), &testTask{}); !errors.Is(err, ErrStopped) {
		t.Fatalf("post-stop admission = %v", err)
	}
	if n, _, _, _, _ := p.Stats(); n != 1 {
		t.Fatalf("submission after stop: %d", n)
	}
}

func TestSubmitUrgentContextWaitsWhenBothLanesFull(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 1)
	for i := 0; i < urgentMinQueueSize+1; i++ {
		if err := p.SubmitUrgentContext(context.Background(), &testTask{}); err != nil {
			t.Fatal(err)
		}
	}
	if main, urgent := p.QueueDepths(); main != 1 || urgent != urgentMinQueueSize {
		t.Fatalf("depths = %d, %d", main, urgent)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.SubmitUrgentContext(ctx, &testTask{}) }()
	waitFor(t, func() bool { return p.WaitingSubmitters() == 1 }, "urgent admission")
	p.Start(context.Background(), func(context.Context, *testTask) error { return nil })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if drained, remaining := p.Stop(time.Second); !drained || remaining != 0 {
		t.Fatalf("stop = %v, %d", drained, remaining)
	}
}

func TestSubmitContextConcurrentStopAccountsEveryAcceptedTask(t *testing.T) {
	for iter := 0; iter < 30; iter++ {
		p := NewWorkerPool[*testTask](2, 2)
		var executed, accepted atomic.Int64
		p.Start(context.Background(), func(context.Context, *testTask) error {
			executed.Add(1)
			return nil
		})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(urgent bool) {
				defer wg.Done()
				<-start
				for j := 0; j < 20; j++ {
					var err error
					if urgent {
						err = p.SubmitUrgentContext(context.Background(), &testTask{})
					} else {
						err = p.SubmitContext(context.Background(), &testTask{})
					}
					if err == nil {
						accepted.Add(1)
					} else if !errors.Is(err, ErrStopped) {
						t.Errorf("unexpected error: %v", err)
					}
				}
			}(i%2 == 0)
		}
		close(start)
		p.Stop(time.Second)
		wg.Wait()
		if executed.Load() != accepted.Load() {
			t.Fatalf("accepted %d but executed %d", accepted.Load(), executed.Load())
		}
		if n, _, _, progress, depth := p.Stats(); n != accepted.Load() || progress != 0 || depth != 0 {
			t.Fatalf("accounting: submitted %d progress %d depth %d", n, progress, depth)
		}
	}
}

func TestPromoteMovesOwnedTaskWithoutDuplicateOrCounterChange(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 3)
	for _, url := range []string{"a", "b", "c"} {
		if err := p.SubmitContext(context.Background(), &testTask{url: url}); err != nil {
			t.Fatal(err)
		}
	}
	if !p.Promote(func(task *testTask) bool { return task.url == "b" }) {
		t.Fatal("queued task was not promoted")
	}
	if p.Promote(func(task *testTask) bool { return task.url == "b" }) {
		t.Fatal("promotion created a duplicate")
	}
	if main, urgent := p.QueueDepths(); main != 2 || urgent != 1 {
		t.Fatalf("depths=%d,%d", main, urgent)
	}
	if n, _, _, _, _ := p.Stats(); n != 3 {
		t.Fatal("promotion counted another submission")
	}
	if got := (<-p.urgentQueue).url; got != "b" {
		t.Fatalf("urgent=%s", got)
	}
	for _, want := range []string{"a", "c"} {
		if got := (<-p.taskQueue).url; got != want {
			t.Fatalf("normal order=%s want %s", got, want)
		}
	}
}

func TestPromoteFullUrgentLaneLeavesTaskInOriginalQueue(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 2)
	for i := 0; i < urgentMinQueueSize; i++ {
		if err := p.SubmitUrgentContext(context.Background(), &testTask{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SubmitContext(context.Background(), &testTask{url: "normal"}); err != nil {
		t.Fatal(err)
	}
	if p.Promote(func(task *testTask) bool { return true }) {
		t.Fatal("full urgent lane accepted promotion")
	}
	if got := (<-p.taskQueue).url; got != "normal" {
		t.Fatalf("task lost or reordered: %s", got)
	}
}

func TestPausedFullQueuesRemainBoundedAndCancelAllProducers(t *testing.T) {
	p := NewWorkerPool[*testTask](2, 3)
	gate := newFakeGate()
	gate.Pause()
	p.SetGate(gate)
	var executed atomic.Int64
	p.Start(context.Background(), func(context.Context, *testTask) error {
		executed.Add(1)
		return nil
	})
	const capacity = 3 + urgentMinQueueSize
	for i := 0; i < capacity; i++ {
		if err := p.SubmitUrgentContext(context.Background(), &testTask{}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const producers = 32
	results := make(chan error, producers)
	for i := 0; i < producers; i++ {
		go func(urgent bool) {
			if urgent {
				results <- p.SubmitUrgentContext(ctx, &testTask{})
			} else {
				results <- p.SubmitContext(ctx, &testTask{})
			}
		}(i%2 == 0)
	}
	waitFor(t, func() bool { return p.WaitingSubmitters() == producers }, "paused producers")
	if main, urgent := p.QueueDepths(); main != 3 || urgent != urgentMinQueueSize {
		t.Fatalf("bounded queue depths = %d/%d", main, urgent)
	}
	if n, _, _, _, _ := p.Stats(); n != capacity || executed.Load() != 0 {
		t.Fatalf("paused work: accepted=%d executed=%d", n, executed.Load())
	}
	cancel()
	for i := 0; i < producers; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled producer: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled producer did not return")
		}
	}
	if drained, remaining := p.Stop(time.Second); !drained || remaining != 0 {
		t.Fatalf("paused stop = %v, %d", drained, remaining)
	}
	if executed.Load() != capacity {
		t.Fatalf("executed = %d, want %d", executed.Load(), capacity)
	}
}

func TestCancelledContextRejectsEvenWithSpareCapacity(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, submit := range []func(context.Context, *testTask) error{p.SubmitContext, p.SubmitUrgentContext} {
		if err := submit(ctx, &testTask{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admission = %v", err)
		}
	}
	if n, _, _, _, depth := p.Stats(); n != 0 || depth != 0 {
		t.Fatalf("cancelled task accepted: submitted=%d depth=%d", n, depth)
	}
	p.Stop(time.Second)
}
