package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"strings"
	"testing"
	"time"
)

type lostBreakerAckRepository struct {
	*memoryBreakerRepository
	lost bool
}

func (r *lostBreakerAckRepository) Save(ctx context.Context, next breakerstore.Snapshot) error {
	r.mu.Lock()
	previous, exists := r.rows[next.Control.ID]
	r.mu.Unlock()
	if exists && previous.Control.Revision == next.Control.Revision && len(previous.Window.Failures) != len(next.Window.Failures) {
		return breakerstore.ErrStale
	}
	if err := r.memoryBreakerRepository.Save(ctx, next); err != nil {
		return err
	}
	if !r.lost {
		r.lost = true
		return errors.New("fixture commit succeeded but acknowledgement lost")
	}
	return nil
}

func TestBreakerReviewLostCommitAckAndWindowExpiryCanRetry(t *testing.T) {
	e, base := newSmartBreakerFixture(t, 1)
	e.breakerStore = &lostBreakerAckRepository{memoryBreakerRepository: base}
	smartFailure(e, 1, "detail", "missing")
	if err := e.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].detector.Snapshot(time.Now().Add(2 * time.Minute))
	e.breakerMu.Unlock()
	flushSmart(t, e)
	e.breakerMu.Lock()
	s := e.smartBreakers["synthetic"]
	defer e.breakerMu.Unlock()
	if s.dirty || s.savedRevision < s.controlRevision {
		t.Fatal("lost acknowledgement left control permanently unsaved")
	}
}

func TestBreakerReviewRetryKeepsProtectionAndCounts(t *testing.T) {
	e, repo := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	flushSmart(t, e)
	old := smartLifecycle(e)
	e.tickBreakers(context.Background(), old.Deadline.Add(time.Second))
	if !e.ResumeSite("synthetic") {
		t.Fatal("retry rejected")
	}
	l := expectSmartState(t, e, "cooldown")
	if e.breakerSum("synthetic") != 3 || !e.isPaused(pauseScope{Site: "synthetic"}) || !strings.Contains(l.Text(), "manual_retry") {
		t.Fatal("retry lost protection, counts or report")
	}
	if done, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "list"}); ok {
		done()
		t.Fatal("retry admitted unchecked stage")
	}
	repo.err = errors.New("fixture unavailable")
	if err := e.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected save failure")
	}
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	expectSmartState(t, e, "cooldown")
	repo.err = nil
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	expectSmartState(t, e, "probing")
}

func TestBreakerReviewManualTakeoverResumeStillVerifiesWithinRun(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	if !e.PauseSite("synthetic", "maintenance") {
		t.Fatal("manual pause failed")
	}
	expectSmartState(t, e, "closed")
	if !e.ResumeSite("synthetic") {
		t.Fatal("ordinary resume rejected")
	}
	expectSmartState(t, e, "cooldown")
	if st, ok := e.pauseStateOfScope(pauseScope{Site: "synthetic"}); !ok || st.Source != PauseSourceBreaker {
		t.Fatal("resume bypassed automatic protection")
	}
	if e.breakerSum("synthetic") != 1 {
		t.Fatal("takeover/resume erased failures")
	}
}

func TestBreakerReviewDeadlineEnforcedWithoutManagerTick(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	for i := 1; i <= 4; i++ {
		smartFailure(e, i, "detail", fmt.Sprint(i))
	}
	flushSmart(t, e)
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].event.Deadline = time.Now().Add(-time.Second)
	e.breakerMu.Unlock()
	if done, ok := e.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		done()
		t.Fatal("expired observation admitted a new task")
	}
	e.tickBreakers(context.Background(), time.Now().Add(-time.Hour))
	expectSmartState(t, e, "manual")
}

func TestBreakerReviewLateHealthyProbeCannotBeatDeadline(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	<-entered
	e.breakerMu.Lock()
	e.smartBreakers["synthetic"].event.Deadline = time.Now().Add(-time.Second)
	e.breakerMu.Unlock()
	close(release)
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "manual")
	if l.Checks[0].State != "interrupted" || e.breakerSum("synthetic") != 1 {
		t.Fatal("late callback certified recovery or erased failure")
	}
}

func TestBreakerReviewRetryDoesNotOverlapTimedOutCallback(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	smartFailure(e, 1, "detail", "missing")
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	<-entered
	e.tickBreakers(context.Background(), smartLifecycle(e).Deadline.Add(time.Second))
	if e.ResumeSite("synthetic") {
		close(release)
		e.tickerWG.Wait()
		t.Fatal("retry overlapped running callback")
	}
	close(release)
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "manual")
	if l.Checks[0].State != "interrupted" {
		t.Fatal("late result changed interrupted report")
	}
	if !e.ResumeSite("synthetic") {
		t.Fatal("retry still rejected after callback exit")
	}
	l = expectSmartState(t, e, "cooldown")
	if l.Checks[0].State != "pending" || e.breakerSum("synthetic") != 1 {
		t.Fatal("retry reused stale proof or erased failures")
	}
}

func TestBreakerReviewTimelineTruncationMustNotInvalidateHealthyStages(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1000)
	var stages []string
	for i := 0; i < 65; i++ {
		stages = append(stages, fmt.Sprintf("stage%02d", i))
	}
	registerBreakerFixtureStages(e, stages...)
	for i := 1; i <= 4; i++ {
		smartFailure(e, i, stages[0], fmt.Sprint(i))
	}
	l := expectSmartState(t, e, "observing")
	flushSmart(t, e)
	for i, stage := range stages {
		for j := 0; j < 3; j++ {
			id := 100 + i*3 + j
			if i == 0 {
				id = j + 1
			}
			task, done := admitSmartTrial(t, e, id, stage)
			task.breakerRound = "verification"
			e.recordSmartOutcome(task, nil)
			done()
		}
	}
	e.breakerMu.Lock()
	s := e.smartBreakers["synthetic"]
	ready := e.breakerStagesReadyLocked(s, 2)
	stageCount := len(s.event.Checks)
	limited := s.event.report.Limited
	e.breakerMu.Unlock()
	if !ready || stageCount != 65 {
		t.Fatal("fixture lacks complete stage proof")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.LastFailureAt.Add(31*time.Second))
	if got := smartLifecycle(e); got.State != "recovered" {
		t.Fatalf("all 65 stages completed three sample checks; report Limited=%v forced state=%s", limited, got.State)
	}
}

func TestBreakerReviewDeadlineMustFinalizeRunningStageReport(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 1)
	registerBreakerFixtureStages(e, "detail")
	smartFailure(e, 1, "detail", "missing")
	entered, release := make(chan struct{}), make(chan struct{})
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		close(entered)
		<-release
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	e.tickBreakers(context.Background(), smartLifecycle(e).Deadline.Add(time.Millisecond))
	close(release)
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "manual")
	if l.Checks[0].State == "probing" || l.Checks[0].FinishedAt.IsZero() {
		t.Fatalf("final incident still has unfinished stage after callback exited: state=%s finished=%v", l.Checks[0].State, l.Checks[0].FinishedAt)
	}
}

func TestBreakerReviewIdleStageAfterSuccessfulProbeCannotFinish(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "idle")
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := expectSmartState(t, e, "ramping")
	flushSmart(t, e)
	task, done := admitSmartTrial(t, e, 10, "detail")
	e.recordSmartOutcome(task, nil)
	done()
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "ramping")
	e.tickBreakers(context.Background(), l.Deadline.Add(time.Millisecond))
	l = expectSmartState(t, e, "manual")
	t.Logf("confirmed idle stage forces manual despite both semantic probes passed: %+v", l.Checks)
}

func TestBreakerReviewReportCleaningMustPreserveValidStageIdentity(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	stage := strings.Repeat("页", 44) // 44 characters fits varchar(128), 132 UTF-8 bytes.
	registerBreakerFixtureStages(e, stage)
	for i := 1; i <= 4; i++ {
		smartFailure(e, i, stage, fmt.Sprint(i))
	}
	flushSmart(t, e)
	for i := 1; i <= 3; i++ {
		task, done := admitSmartTrial(t, e, i, stage)
		task.breakerRound = "verification"
		e.recordSmartOutcome(task, nil)
		done()
	}
	l := smartLifecycle(e)
	if l.Evidence[0].Stage != stage || l.Groups[0].Stage != stage {
		t.Fatal("valid stage identity lost in probe evidence/report grouping")
	}
	if l.Checks[0].Stage != stage || l.Checks[0].TaskSuccesses != 3 {
		t.Fatalf("valid stage key changed: input bytes=%d stored bytes=%d task successes=%d", len(stage), len(l.Checks[0].Stage), l.Checks[0].TaskSuccesses)
	}
}

func TestBreakerReviewExplicitForceResumeBypassesAllStageProof(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	flushSmart(t, e)
	if !e.ForceResumeSite("synthetic") {
		t.Fatal("resume did not clear pause")
	}
	flushSmart(t, e)
	_, done1 := admitSmartTrial(t, e, 10, "detail")
	defer done1()
	_, done2 := admitSmartTrial(t, e, 11, "list")
	defer done2()
	active, err := r.LoadActive(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("unexpected active reports: %v %v", active, err)
	}
	t.Logf("confirmed unverified manual closure admits both stages concurrently and is absent from startup restore; failure count=%d", e.breakerSum("synthetic"))
}

func TestBreakerReviewRestartResetsAllStageProofAndKeepsPause(t *testing.T) {
	e, r := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail", "list")
	for i := 1; i <= 3; i++ {
		smartFailure(e, i, "detail", "missing")
	}
	startSmartProbe(t, e, func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error) {
		return BreakerProbeResult{Healthy: true, Semantic: true}, nil
	})
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	flushSmart(t, e)
	task, done := admitSmartTrial(t, e, 10, "detail")
	e.recordSmartOutcome(task, nil)
	done()
	flushSmart(t, e)
	restarted, _ := newSmartBreakerFixture(t, 100)
	restarted.breakerStore, restarted.breakerRuntime = r, false
	registerBreakerFixtureStages(restarted, "detail", "list")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); restarted.tickerWG.Wait() }()
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	l := expectSmartState(t, restarted, "cooldown")
	for _, check := range l.Checks {
		if check.State != "pending" || check.TaskSuccesses != 0 {
			t.Fatalf("stale stage proof restored: %+v", check)
		}
		if done, ok := restarted.admitBreakerTask(&Task{Site: "synthetic", Stage: check.Stage}); ok {
			done()
			t.Fatal("restarted stage admitted")
		}
	}
	if restarted.breakerSum("synthetic") != 3 {
		t.Fatal("restart erased failure count before verification")
	}
	t.Logf("confirmed restart refuses stale proof; runtime failure count=%d, persisted window failures=%d", restarted.breakerSum("synthetic"), l.WindowFailures)
}
