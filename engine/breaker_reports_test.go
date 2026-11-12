package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

var testBreakerRound atomic.Uint64

// Older execution-path fixtures send terminal notifications directly. Each call
// represents a distinct claimed round; detector dedup tests use recordSmartOutcome.
func (e *Engine) testBreakerTask(task *Task) *Task {
	copy := *task
	e.breakers.breakerMu.Lock()
	if s := e.breakers.smartBreakers[task.Site]; s != nil {
		copy.breakerEpoch = s.epoch
	}
	e.breakers.breakerMu.Unlock()
	copy.breakerRound = fmt.Sprint(testBreakerRound.Add(1))
	return &copy
}
func (e *Engine) recordTestBreakerFailure(task *Task, failure ...error) (BreakerStatus, bool) {
	st, tripped := e.recordBreakerFailure(e.testBreakerTask(task), failure...)
	if err := e.breakers.flushBreakerReports(context.Background()); err != nil {
		panic(err)
	}
	return st, tripped
}
func (e *Engine) notifyTestFailure(ctx context.Context, task *Task, err error) {
	e.notifyFailure(ctx, e.testBreakerTask(task), err)
	if err := e.breakers.flushBreakerReports(context.Background()); err != nil {
		panic(err)
	}
}

func TestBreakerReportsUseOneIDAndRetainHistory(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 2})
	for i := 0; i < 2; i++ {
		e.notifyTestFailure(context.Background(), &Task{Site: "a", Stage: "detail", ID: i + 1, URL: "https://example.test/?token=hidden"}, errors.New("HTTP 429"))
	}
	first, err := e.BreakerReports(context.Background(), "a")
	if err != nil || len(first) != 1 {
		t.Fatalf("reports %+v %v", first, err)
	}
	id := first[0].Capture.ID
	for _, ev := range col.snapshot() {
		if ev.Type == events.TypeBreakerTripped && ev.Attrs["incident_id"] != id {
			t.Fatalf("event ID %q differs from report %q", ev.Attrs["incident_id"], id)
		}
	}
	if len(first[0].Evidence) != 2 || strings.Contains(first[0].Evidence[0].URL, "hidden") {
		t.Fatalf("evidence: %+v", first[0])
	}
	first[0].Evidence[0].Error = "mutated"
	if !e.ForceResumeSite("a") {
		t.Fatal("resume")
	}
	flushSmart(t, e)
	for i := 0; i < 2; i++ {
		e.notifyTestFailure(context.Background(), &Task{Site: "a", Stage: "detail", ID: i + 3}, errors.New("HTTP 403"))
	}
	all, err := e.BreakerReports(context.Background(), "a")
	if err != nil || len(all) != 2 {
		t.Fatalf("history %+v %v", all, err)
	}
	if all[1].Capture.ID != id || all[1].State != "closed" || all[1].Evidence[0].Error == "mutated" || all[0].Capture.ID == id {
		t.Fatalf("history mutated: %+v", all)
	}
}

func TestBreakerDetectorOwnsBoundedDeduplicatedEvidence(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Hour, Threshold: incident.MaxSamples + 1, Policy: breaker.Policy{FastThreshold: 1000, MixedThreshold: 1000}})
	for i := 0; i <= incident.MaxSamples; i++ {
		e.recordTestBreakerFailure(&Task{Site: "a", ID: i + 1, Stage: "detail"}, errors.New("timeout"))
	}
	reports, _ := e.BreakerReports(context.Background(), "a")
	if len(reports) != 1 || len(reports[0].Evidence) != incident.MaxSamples || !reports[0].Limited {
		t.Fatalf("bounded evidence: %+v", reports)
	}
	task := &Task{Site: "a", ID: 999, Stage: "detail", breakerRound: "one"}
	e.recordSmartOutcome(task, errors.New("once"))
	e.recordSmartOutcome(task, errors.New("duplicate"))
	if e.breakerSum("a") != incident.MaxSamples+2 {
		t.Fatal("status must read the deduplicated detector count")
	}
}

func TestBreakerReportConcurrentReadResumeFailure(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Threshold: 2})
	var wg sync.WaitGroup
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				switch w {
				case 0:
					e.recordTestBreakerFailure(&Task{Site: "a", ID: i + 1}, errors.New("timeout"))
				case 1:
					e.ForceResumeSite("a")
				case 2:
					_, err := e.BreakerReports(context.Background(), "a")
					if err != nil {
						t.Error(err)
					}
				}
			}
		}(n)
	}
	wg.Wait()
}

func TestManualTakeoverDoesNotRestoreAfterImmediateStop(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	r := &memoryBreakerRepository{}
	e.breakers.breakerStore = r
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 100, Policy: breaker.Policy{FastThreshold: 3}})
	for i := 0; i < 3; i++ {
		e.recordTestBreakerFailure(&Task{Site: "a", Stage: "detail", ID: i + 1}, errors.New("missing"))
	}
	if !e.PauseSite("a", "operator") {
		t.Fatal("takeover failed")
	}
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("stop did not flush closure")
	}
	active, err := r.LoadActive(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("manual takeover remained restorable: %+v %v", active, err)
	}
	restarted := &Engine{breakers: breakerCoordinator{breakerStore: r}}
	restarted.SetSiteBreaker("a", breaker.Config{Enabled: true, Threshold: 100})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); restarted.tickerWG.Wait() }()
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if restarted.isPaused(pauseScope{Site: "a", Stage: "detail"}) || restarted.isPaused(pauseScope{Site: "a", Stage: "list"}) {
		t.Fatal("manual pause survived restart")
	}
	reports, err := restarted.BreakerReports(ctx, "a")
	if err != nil || len(reports) != 1 || reports[0].State != "closed" {
		t.Fatalf("takeover history lost: %+v %v", reports, err)
	}
}

func TestNarrowManualPauseDoesNotOwnSiteProtection(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	flushSmart(t, e)
	if !e.PauseStage("synthetic", "list", "operator") {
		t.Fatal("narrow manual pause failed")
	}
	if got := smartLifecycle(e).State; got != "cooldown" {
		t.Fatalf("narrow pause closed broader automatic event: %s", got)
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	if !e.isPaused(pauseScope{Site: "synthetic", Stage: "list"}) || e.isPaused(pauseScope{Site: "synthetic", Stage: "detail"}) {
		t.Fatal("automatic release erased manual scope")
	}
}
