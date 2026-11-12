package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

func mixedSampleFixture(t *testing.T, n int) *Engine {
	t.Helper()
	e, _ := newSmartBreakerFixture(t, 100)
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Minute, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: n}})
	registerBreakerFixtureStages(e, "detail")
	for i := 1; i <= n; i++ {
		smartFailure(e, i, "detail", fmt.Sprint(i))
	}
	expectSmartState(t, e, "observing")
	flushSmart(t, e)
	return e
}

func verificationSuccess(t *testing.T, e *Engine, id int, round string) {
	t.Helper()
	task, done := admitSmartTrial(t, e, id, "detail")
	task.breakerRound = round
	e.recordSmartOutcome(task, nil)
	done()
}

func TestBreakerMixedThreeSelectedSamplesMatchReport(t *testing.T) {
	e := mixedSampleFixture(t, 4)
	verificationSuccess(t, e, 80, "other")
	for i := 0; i < 3; i++ {
		verificationSuccess(t, e, 1, fmt.Sprint("retry", i))
	}
	l := smartLifecycle(e)
	if l.Checks[0].TaskSuccesses != 1 || l.Checks[0].Verified {
		t.Fatalf("same sample counted as three: %+v", l.Checks[0])
	}
	verificationSuccess(t, e, 2, "retry")
	l = smartLifecycle(e)
	if l.Checks[0].Verified {
		t.Fatal("two samples passed")
	}
	verificationSuccess(t, e, 3, "retry")
	l = smartLifecycle(e)
	if !l.Checks[0].Verified || l.Checks[0].TaskSuccesses != 3 {
		t.Fatal("three selected samples missing")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.LastFailureAt.Add(31*time.Second))
	expectSmartState(t, e, "recovered")
}

func TestBreakerMixedFewSamplesRotateThreeFreshRounds(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			e := mixedSampleFixture(t, n)
			ids := []int{1, 1, 1}
			if n == 2 {
				ids = []int{1, 2, 1}
			}
			for pass, id := range ids {
				verificationSuccess(t, e, id, fmt.Sprintf("retry-%d", pass))
				// A duplicate outcome is not another execution.
				smartSuccess(e, &Task{ID: id, Site: "synthetic", Stage: "detail", breakerRound: fmt.Sprintf("retry-%d", pass)})
				l := smartLifecycle(e)
				if l.Checks[0].TaskSuccesses != pass+1 || l.Checks[0].Verified != (pass == 2) {
					t.Fatalf("wrong proof: %+v", l.Checks[0])
				}
			}
		})
	}
}

func TestBreakerMixedProbeMustConfirmSelectedSampleAndThreePasses(t *testing.T) {
	e := mixedSampleFixture(t, 4)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.LastFailureAt.Add(31*time.Second))
	var selected []string
	startSmartProbe(t, e, func(_ context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		selected = append(selected, req.SampleID)
		return BreakerProbeResult{Healthy: true, Semantic: true, Complete: true, Detail: "本次指定样本的完整语义检查", SampleID: req.SampleID}, nil
	})
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l = expectSmartState(t, e, "ramping")
	if strings.Join(selected, ",") != "task:1,task:2,task:3" || l.Checks[0].ProbeSuccesses != 3 || l.Checks[0].Verified {
		t.Fatalf("sample selection/report mismatch: %v %+v", selected, l.Checks)
	}
}

func TestBreakerMixedProbeCannotIgnoreRequestedFailureSample(t *testing.T) {
	e := mixedSampleFixture(t, 4)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.LastFailureAt.Add(31*time.Second))
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	l = expectSmartState(t, e, "cooldown")
	if l.Checks[0].ProbeSuccesses != 0 || l.Checks[0].Verified {
		t.Fatal("generic successful probe silently substituted another sample")
	}
}

func TestBreakerHourlyCycleCanAutoRecoverAndPreserveManualPause(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	flushSmart(t, e)
	l = smartLifecycle(e)
	e.tickBreakers(context.Background(), l.RecheckAt)
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	flushSmart(t, e)
	l = expectSmartState(t, e, "ramping")
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	l = expectSmartState(t, e, "recovered")
	if !l.RecheckAt.IsZero() || e.breakerSum("synthetic") != 0 || !strings.Contains(l.Text(), "hourly_recheck") {
		t.Fatal("hourly recovery did not finish normally")
	}
	e2, _ := readOnlyFixture(t, "detail")
	l = smartLifecycle(e2)
	e2.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	flushSmart(t, e2)
	if !e2.PauseSite("synthetic", "maintenance") {
		t.Fatal("pause failed")
	}
	flushSmart(t, e2)
	e2.tickBreakers(context.Background(), time.Now().Add(10*time.Hour))
	if !e2.isPaused(pauseScope{Site: "synthetic"}) {
		t.Fatal("hourly tick removed manual pause")
	}
}

func TestBreakerUnfinishedProbeBlocksHourlyRecheck(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true, Complete: true, Detail: "late"}, nil
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	flushSmart(t, e)
	l = smartLifecycle(e)
	e.tickBreakers(context.Background(), l.RecheckAt)
	l = expectSmartState(t, e, "manual")
	if !strings.Contains(l.Text(), "hourly_recheck_skipped") {
		t.Fatal("stuck callback had no hourly status record")
	}
	close(release)
	e.tickerWG.Wait()
	expectSmartState(t, e, "manual")
}

func TestBreakerHistoryBackpressureCannotResetFailures(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	flushSmart(t, e)
	l := smartLifecycle(e)
	e.breakerMu.Lock()
	// Exercise the capacity boundary without spending the one-minute detector
	// window constructing thousands of presentation records under -race.
	e.smartBreakers["synthetic"].event.pendingHistory = make([]incident.Transition, 4095)
	e.breakerMu.Unlock()
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "manual")
	if e.breakerSum("synthetic") != 3 {
		t.Fatal("history overflow reset failure evidence")
	}
}

func TestBreakerHistoryPressureCannotDeadlockManualPauseOrReopenClosedEvent(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	e.breakerMu.Lock()
	s := e.smartBreakers["synthetic"]
	// Queue pressure is independent of history payload content.
	s.event.pendingHistory = make([]incident.Transition, 4096)
	e.breakerMu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- e.PauseSite("synthetic", "maintenance") }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("manual pause failed")
		}
	case <-time.After(time.Second):
		t.Fatal("manual pause deadlocked on history pressure")
	}
	l := expectSmartState(t, e, "closed")
	if !e.isPaused(pauseScope{Site: "synthetic"}) || l.FinalAt.IsZero() {
		t.Fatal("history pressure reopened manual closure")
	}
}

func TestBreakerHourlyRecheckKeepsPauseAndGrantsOnlyOneCycle(t *testing.T) {
	e, r := readOnlyFixture(t, "detail")
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	l = expectSmartState(t, e, "manual")
	if l.RecheckAt.IsZero() {
		t.Fatal("missing hourly schedule")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.RecheckAt.Add(-time.Second))
	expectSmartState(t, e, "manual")
	e.tickBreakers(context.Background(), l.RecheckAt.Add(3*time.Hour))
	next := expectSmartState(t, e, "cooldown")
	if next.Cycle != l.Cycle+1 || next.Checks[0].ProbeSuccesses != 0 || !e.isPaused(pauseScope{Site: "synthetic"}) {
		t.Fatal("missed hours caused catchup or lost protection")
	}
	r.err = errors.New("fixture write unavailable")
	if err := e.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected save error")
	}
	e.tickBreakers(context.Background(), next.NextProbeAt.Add(time.Millisecond))
	expectSmartState(t, e, "cooldown")
}

func TestBreakerHourlyRestartPreservesDueTimeAndRecordsOldScope(t *testing.T) {
	e, r := readOnlyFixture(t, "detail")
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := smartLifecycle(e)
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	want := smartLifecycle(e).RecheckAt
	flushSmart(t, e)
	restarted, _ := newSmartBreakerFixture(t, 100)
	restarted.breakerStore, restarted.breakerRuntime = r, false
	registerBreakerFixtureStages(restarted, "detail") // declaration removed in new process
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); restarted.tickerWG.Wait() }()
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	l = expectSmartState(t, restarted, "manual")
	if !l.RecheckAt.Equal(want) || l.Checks[0].Verified || l.Checks[0].Verification != "task" {
		t.Fatal("restart changed schedule or reused proof")
	}
	if !strings.Contains(l.Text(), fixtureReadOnlyCoverage) || !strings.Contains(l.Text(), "process_interrupted") {
		t.Fatal("restart erased history")
	}
}

func TestBreakerSelectedTaskRetryMySQLUsesExistingVersionedPath(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.breakerStore = &memoryBreakerRepository{}
	e.breakerRuntime = true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Minute, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 2}})
	for i := 1; i <= 2; i++ {
		row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/%d", i), Status: models.TaskStatusFailed, CompensationMode: ""}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		smartFailure(e, int(row.ID), "detail", fmt.Sprint(i))
	}
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	var rows []models.CrawlerTask
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != models.TaskStatusPending || rows[0].Reprocess != 1 || rows[1].Status != models.TaskStatusFailed {
		t.Fatalf("targeted retry failed: status=%d reprocess=%d second=%d", rows[0].Status, rows[0].Reprocess, rows[1].Status)
	}
	e.retryBreakerSamples(context.Background())
	if err := db.First(&rows[0], rows[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].Reprocess != 1 {
		t.Fatal("pending sample requeued twice")
	}
}
