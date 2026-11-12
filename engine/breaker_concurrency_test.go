package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
)

func TestSmartBreakerEvidenceUpdatesDoNotStarveProbe(t *testing.T) {
	e, base := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	id := 100
	e.breakers.breakerStore = &reviewInterleavingRepository{memoryBreakerRepository: base, duringSave: func() {
		id++
		smartSuccess(e, &Task{ID: id, Site: "synthetic", Stage: "list", breakerRound: "0:0"})
	}}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	flushSmart(t, e) // newer evidence arrives during every write, including this one
	e.tickBreakers(context.Background(), time.Now())
	expectSmartState(t, e, "ramping")
	if e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("newer evidence blocked an already durable ramp transition")
	}
}

type blockingBreakerRepository struct {
	*memoryBreakerRepository
	entered chan struct{}
	once    sync.Once
}

func (r *blockingBreakerRepository) Save(ctx context.Context, _ breakerstore.Snapshot) error {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func TestSmartBreakerBlockedWriterCannotDelayDeadline(t *testing.T) {
	r := &blockingBreakerRepository{memoryBreakerRepository: &memoryBreakerRepository{}, entered: make(chan struct{})}
	e := &Engine{breakers: breakerCoordinator{breakerStore: r}}
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); e.tickerWG.Wait() })
	if err := e.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	smartFailure(e, 1, "detail", "missing")
	select {
	case <-r.entered:
	case <-time.After(time.Second):
		t.Fatal("report writer did not start")
	}
	e.breakers.breakerMu.Lock()
	e.breakers.smartBreakers["synthetic"].event.Deadline = time.Now().Add(-time.Second)
	e.breakers.breakerMu.Unlock()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for smartLifecycle(e).State != "manual" {
		select {
		case <-timeout.C:
			t.Fatal("blocked database write stalled the independent deadline timer")
		case <-ticker.C:
		}
	}
	// Waiting for the writer slot also obeys the caller's deadline.
	flushCtx, flushCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer flushCancel()
	if err := e.breakers.flushBreakerReports(flushCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer gate ignored cancellation: %v", err)
	}
}

func TestSmartBreakerObservationRequiresAffectedStagesAndFreshSuccess(t *testing.T) {
	for _, mode := range []string{"unrelated", "one-page", "new-failure", "covered"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := newSmartBreakerFixture(t, 100)
			for i := 1; i <= 4; i++ {
				stage := "detail"
				if i > 2 {
					stage = "list"
				}
				smartFailure(e, i, stage, fmt.Sprint(i))
			}
			l := expectSmartState(t, e, "observing")
			for i := 10; i < 16; i++ {
				stage, id := "detail", i
				if i >= 13 {
					stage = "list"
				}
				if mode == "unrelated" {
					stage = "other"
				}
				if mode == "one-page" {
					id = 50
				}
				if mode == "covered" || mode == "new-failure" {
					id = []int{1, 2, 1, 3, 4, 3}[i-10]
				}
				if mode == "covered" || mode == "new-failure" {
					task, done := admitSmartTrial(t, e, id, stage)
					task.breakerRound = fmt.Sprint(i)
					e.recordSmartOutcome(task, nil)
					done()
				} else {
					smartSuccess(e, &Task{ID: id, Site: "synthetic", Stage: stage, breakerRound: fmt.Sprint(i)})
				}
			}
			if mode == "new-failure" {
				smartFailure(e, 99, "detail", "new fault after successes")
			}
			flushSmart(t, e)
			e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
			want := "observing" // serial failed samples retain their bounded validation opportunity
			if mode == "covered" {
				want = "recovered"
			}
			expectSmartState(t, e, want)
			failures := 4
			if mode == "new-failure" {
				failures++
			}
			if mode == "covered" {
				failures = 0
			}
			if got := e.breakerSum("synthetic"); got != failures {
				t.Fatalf("observation erased hard-window failures: %d", got)
			}
		})
	}
}

func TestSmartBreakerStageResumeCannotBypassSiteProtection(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	other := &Task{ID: 50, Site: "synthetic", Stage: "list", breakerRound: "0:0"}
	done, ok := e.breakers.admitBreakerTask(other)
	if !ok {
		t.Fatal("other stage admission failed")
	}
	defer done()
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	if e.ResumeStage("synthetic", "detail") {
		t.Fatal("stage resume bypassed site protection")
	}
	e.recordSmartOutcome(other, errors.New("list operation failed"))
	if got := e.breakerSum("synthetic"); got != 4 {
		t.Fatalf("manual stage recovery discarded terminal outcomes: %d", got)
	}
}

func TestSmartBreakerRestartDuringRampReturnsToCooldown(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	flushSmart(t, e)
	want := expectSmartState(t, e, "ramping")
	restarted := &Engine{breakers: breakerCoordinator{breakerStore: r}}
	restarted.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); restarted.tickerWG.Wait() })
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	got := expectSmartState(t, restarted, "cooldown")
	if got.ProbeAttempts != want.ProbeAttempts || !got.Deadline.Equal(want.Deadline) || !got.LastFailureAt.Equal(want.LastFailureAt) {
		t.Fatal("restart lost recovery budget or observation evidence")
	}
	if _, ok := restarted.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		t.Fatal("restart trusted unfinished real-task recovery")
	}
	if _, ok := restarted.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "list"}); ok {
		t.Fatal("restart reopened a stage before site verification")
	}
}

func TestSmartBreakerSiteProbeWaitsForPriorInflight(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 4)
	other := &Task{ID: 50, Site: "synthetic", Stage: "list", breakerRound: "0:0"}
	done, ok := e.breakers.admitBreakerTask(other)
	if !ok {
		t.Fatal("admission")
	}
	defer done()
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, "detail", "missing")
	}
	flushSmart(t, e)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	expectSmartState(t, e, "cooldown")
	e.recordSmartOutcome(other, errors.New("late failure"))
	done()
	if e.breakerSum("synthetic") != 4 {
		t.Fatal("late failure disappeared")
	}
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) {
		t.Fatal("site protection lost")
	}
}

type timedBreakerRepository struct {
	*memoryBreakerRepository
	writes chan time.Time
	update func()
	failed bool
}

func (r *timedBreakerRepository) Save(ctx context.Context, l breakerstore.Snapshot) error {
	select {
	case r.writes <- time.Now():
	default:
	}
	r.update() // sustained traffic keeps a newer evidence revision pending
	if !r.failed {
		r.failed = true
		return errors.New("synthetic first write failed")
	}
	return r.memoryBreakerRepository.Save(ctx, l)
}

func TestSmartBreakerWriterTimesNormalFlushAndFailureRetry(t *testing.T) {
	e, base := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	id := 100
	r := &timedBreakerRepository{memoryBreakerRepository: base, writes: make(chan time.Time, 4), update: func() {
		id++
		smartSuccess(e, &Task{ID: id, Site: "synthetic", Stage: "list", breakerRound: "0:0"})
	}}
	e.breakers.breakerStore = r
	ctx, cancel := context.WithCancel(context.Background())
	e.tickerWG.Add(1)
	go e.writeBreakerReports(ctx)
	t.Cleanup(func() { cancel(); e.tickerWG.Wait() })
	var stamps []time.Time
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for len(stamps) < 3 {
		select {
		case at := <-r.writes:
			stamps = append(stamps, at)
		case <-timer.C:
			t.Fatal("sustained evidence updates postponed report writes indefinitely")
		}
	}
	if stamps[1].Sub(stamps[0]) < 900*time.Millisecond || stamps[2].Sub(stamps[1]) < 150*time.Millisecond {
		t.Fatalf("writer retried without the normal/failure timer waits: %v", stamps)
	}
}
