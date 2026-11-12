package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/governance"
)

const fixtureReadOnlyCoverage = "读取合成样本，执行完整解析与必要字段校验；本阶段没有写入、下载和派发"

func TestBreakerReadOnlySecondCheckHonorsNewRetryAfter(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	g, err := governance.New(governance.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(g, nil)
	var calls atomic.Int32
	startSmartProbe(t, e, func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		calls.Add(1)
		return completeReadOnlyProbe(ctx, req)
	})
	e.tickerWG.Wait()
	l := smartLifecycle(e)
	until := time.Now().Add(10 * time.Second)
	g.RestoreRetryAfter("synthetic", until)
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	e.tickerWG.Wait()
	l = expectSmartState(t, e, "probing")
	if calls.Load() != 1 || l.NextProbeAt.Before(until) || l.Checks[0].Verified {
		t.Fatal("second check ignored new Retry-After")
	}
	completeSmartProbeRound(t, e)
	expectSmartState(t, e, "ramping")
	if calls.Load() != 2 {
		t.Fatal("second check did not resume after remote cooldown")
	}
}

func TestBreakerReadOnlySecondProbeDoesNotOverlapOrOverrideForceResume(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	startSmartProbe(t, e, func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		calls.Add(1)
		if req.Pass == 2 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return BreakerProbeResult{}, ctx.Err()
			}
		}
		return completeReadOnlyProbe(ctx, req)
	})
	e.tickerWG.Wait()
	l := smartLifecycle(e)
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second probe did not start")
	}
	for i := 0; i < 5; i++ {
		e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Second))
	}
	if calls.Load() != 2 {
		t.Fatal("probe overlap")
	}
	if e.ResumeSite("synthetic") {
		t.Fatal("retry accepted while probe running")
	}
	if !e.ForceResumeSite("synthetic") {
		t.Fatal("force resume failed")
	}
	close(release)
	e.tickerWG.Wait()
	l = expectSmartState(t, e, "closed").Clean()
	if l.Checks[0].Verified || e.breakerSum("synthetic") != 3 {
		t.Fatal("late second success certified health or cleared counts")
	}
}

func TestBreakerReadOnlyMySQLRestoresReportButInvalidatesControlProof(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
		t.Fatal(err)
	}
	e, _ := readOnlyFixture(t, "detail")
	e.breakers.breakerStore = breakerstore.New(db)
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	flushSmart(t, e)
	reports, err := e.breakers.breakerStore.List(context.Background(), "synthetic", 10)
	if err != nil || len(reports) != 1 {
		t.Fatalf("report load failed: %v", err)
	}
	c := reports[0].Checks[0]
	if !c.Verified || c.Verification != "readonly" || c.Coverage != fixtureReadOnlyCoverage || c.ProbeSuccesses != 2 {
		t.Fatalf("persisted proof missing: %+v", c)
	}
	restarted, _ := newSmartBreakerFixture(t, 100)
	restarted.breakers.breakerStore, restarted.breakers.breakerRuntime = breakerstore.New(db), false
	registerBreakerFixtureStages(restarted, "detail")
	restarted.stages[core.StageKey("synthetic", "detail")].config.BreakerReadOnlyCoverage = fixtureReadOnlyCoverage
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); restarted.tickerWG.Wait() }()
	if err := restarted.InitBreakerRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	l := expectSmartState(t, restarted, "cooldown").Clean()
	if l.Checks[0].Verified || l.Checks[0].ProbeSuccesses != 0 || restarted.breakerSum("synthetic") != 3 {
		t.Fatalf("MySQL restart reused proof: %+v", l.Checks)
	}
}

func TestBreakerReadOnlyIdleStageRecoversWithoutTask(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := expectSmartState(t, e, "ramping")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	l = expectSmartState(t, e, "recovered").Clean()
	if l.Checks[0].TaskSuccesses != 0 || !l.Checks[0].Verified || e.breakerSum("synthetic") != 0 {
		t.Fatal("idle complete verification failed")
	}
}

func TestBreakerReadOnlyDeclarationDoesNotMislabelObservedTaskProof(t *testing.T) {
	e, _ := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, "detail")
	e.stages[core.StageKey("synthetic", "detail")].config.BreakerReadOnlyCoverage = fixtureReadOnlyCoverage
	for i, code := range []string{"a", "b", "c", "d"} {
		smartFailure(e, i+1, "detail", code)
	}
	l := expectSmartState(t, e, "observing")
	flushSmart(t, e)
	for i := 1; i <= 3; i++ {
		task, done := admitSmartTrial(t, e, i, "detail")
		task.breakerRound = "verification"
		e.recordSmartOutcome(task, nil)
		done()
	}
	e.tickBreakers(context.Background(), l.LastFailureAt.Add(31*time.Second))
	l = expectSmartState(t, e, "recovered").Clean()
	if l.Checks[0].Verification != "task" || l.Checks[0].ProbeSuccesses != 0 || !l.Checks[0].Verified {
		t.Fatalf("mislabelled task proof: %+v", l.Checks)
	}
}

func readOnlyFixture(t *testing.T, stages ...string) (*Engine, *memoryBreakerRepository) {
	t.Helper()
	e, r := newSmartBreakerFixture(t, 100)
	registerBreakerFixtureStages(e, stages...)
	e.stages[core.StageKey("synthetic", stages[0])].config.BreakerReadOnlyCoverage = fixtureReadOnlyCoverage
	for id := 1; id <= 3; id++ {
		smartFailure(e, id, stages[0], "missing")
	}
	return e, r
}

func completeReadOnlyProbe(_ context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
	return BreakerProbeResult{Healthy: true, Semantic: true, Complete: true, Detail: "已重新读取样本并完成全部字段语义校验"}, nil
}

func TestBreakerReadOnlyRequiresTwoSpacedChecksAndDurableFinish(t *testing.T) {
	e, r := readOnlyFixture(t, "detail")
	var passes []int
	startSmartProbe(t, e, func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
		passes = append(passes, req.Pass)
		if req.Coverage != fixtureReadOnlyCoverage {
			t.Errorf("coverage missing: %+v", req)
		}
		return completeReadOnlyProbe(ctx, req)
	})
	e.tickerWG.Wait()
	l := expectSmartState(t, e, "probing")
	if len(passes) != 1 || l.Checks[0].ProbeSuccesses != 1 || l.Clean().Checks[0].Verified {
		t.Fatalf("first check falsely passed: %+v", l.Checks)
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(-time.Millisecond))
	e.tickerWG.Wait()
	if len(passes) != 1 {
		t.Fatal("second probe ignored interval")
	}
	completeSmartProbeRound(t, e)
	l = expectSmartState(t, e, "ramping")
	if len(passes) != 2 || passes[0] != 1 || passes[1] != 2 || !l.Clean().Checks[0].Verified || l.Checks[0].TaskSuccesses != 0 {
		t.Fatalf("wrong proof: %+v %v", l.Checks, passes)
	}
	flushSmart(t, e)
	slow, done := admitSmartTrial(t, e, 99, "detail")
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "ramping")
	e.recordSmartOutcome(slow, nil)
	done()
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	final := expectSmartState(t, e, "recovered").Clean()
	if e.breakerSum("synthetic") != 0 || !strings.Contains(final.Text(), fixtureReadOnlyCoverage) {
		t.Fatal("missing reset or coverage report")
	}
	r.err = errors.New("fixture final save failure")
	if err := e.breakers.flushBreakerReports(context.Background()); err == nil {
		t.Fatal("expected save failure")
	}
	if release, ok := e.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
		release()
		t.Fatal("unsaved recovery admitted task")
	}
	r.err = nil
	flushSmart(t, e)
	_, release := admitSmartTrial(t, e, 100, "detail")
	release()
}

func TestBreakerReadOnlyMixedStagesKeepIdleTaskStageUnverified(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail", "write")
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	l := expectSmartState(t, e, "ramping")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	l = expectSmartState(t, e, "ramping").Clean()
	if !l.Checks[0].Verified || l.Checks[1].Verified || l.Checks[1].Verification != "task" || l.Checks[1].ProbeSuccesses != 1 {
		t.Fatalf("ordinary probe replaced real task: %+v", l.Checks)
	}
	if e.breakerSum("synthetic") != 3 {
		t.Fatal("partial proof cleared failures")
	}
	task, done := admitSmartTrial(t, e, 50, "write")
	e.recordSmartOutcome(task, nil)
	done()
	e.tickBreakers(context.Background(), l.UpdatedAt.Add(31*time.Second))
	expectSmartState(t, e, "recovered")
}

func TestBreakerReadOnlyRejectsIncompleteAndFailedSecondCheck(t *testing.T) {
	for _, kind := range []string{"partial", "blank", "second_error", "second_partial"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := readOnlyFixture(t, "detail")
			startSmartProbe(t, e, func(ctx context.Context, req BreakerProbeRequest) (BreakerProbeResult, error) {
				result, _ := completeReadOnlyProbe(ctx, req)
				if kind == "partial" || (kind == "second_partial" && req.Pass == 2) {
					result.Complete = false
				}
				if kind == "blank" {
					result.Detail = "  "
				}
				if kind == "second_error" && req.Pass == 2 {
					return result, errors.New("fixture second sample failed")
				}
				return result, nil
			})
			e.tickerWG.Wait()
			completeSmartProbeRound(t, e)
			l := expectSmartState(t, e, "cooldown")
			if l.Clean().Checks[0].Verified || e.breakerSum("synthetic") != 3 {
				t.Fatalf("invalid proof recovered: %+v", l.Checks)
			}
			flushSmart(t, e)
			e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
			l = expectSmartState(t, e, "probing")
			if l.Checks[0].ProbeSuccesses != 0 || l.Checks[0].TaskSuccesses != 0 {
				t.Fatal("retry reused proof")
			}
		})
	}
}

func TestBreakerReadOnlyRealFailureInvalidatesRecovery(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	startSmartProbe(t, e, completeReadOnlyProbe)
	e.tickerWG.Wait()
	completeSmartProbeRound(t, e)
	flushSmart(t, e)
	task, done := admitSmartTrial(t, e, 60, "detail")
	e.recordSmartOutcome(task, core.WithFailureCode(errors.New("fixture real failure"), "parse", "missing"))
	done()
	l := expectSmartState(t, e, "cooldown")
	if e.breakerSum("synthetic") != 4 {
		t.Fatal("late failure was discarded")
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.NextProbeAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "probing")
	if l.Checks[0].ProbeSuccesses != 0 {
		t.Fatal("real failure reused readonly proof")
	}
}

func TestBreakerReadOnlyRestartDiscardsProofAndUsesCurrentDeclaration(t *testing.T) {
	for _, keep := range []bool{true, false} {
		t.Run(map[bool]string{true: "retain_declaration", false: "remove_declaration"}[keep], func(t *testing.T) {
			e, r := readOnlyFixture(t, "detail")
			startSmartProbe(t, e, completeReadOnlyProbe)
			e.tickerWG.Wait()
			flushSmart(t, e)
			restarted, _ := newSmartBreakerFixture(t, 100)
			restarted.breakers.breakerStore, restarted.breakers.breakerRuntime = r, false
			registerBreakerFixtureStages(restarted, "detail")
			if keep {
				restarted.stages[core.StageKey("synthetic", "detail")].config.BreakerReadOnlyCoverage = fixtureReadOnlyCoverage
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); restarted.tickerWG.Wait() }()
			if err := restarted.InitBreakerRuntime(ctx); err != nil {
				t.Fatal(err)
			}
			l := expectSmartState(t, restarted, "cooldown").Clean()
			c := l.Checks[0]
			if c.ProbeSuccesses != 0 || c.TaskSuccesses != 0 || c.Verified || (c.Verification == "readonly") != keep || restarted.breakerSum("synthetic") != 3 {
				t.Fatalf("restart trusted stale proof: %+v", c)
			}
			if release, ok := restarted.breakers.admitBreakerTask(&Task{Site: "synthetic", Stage: "detail"}); ok {
				release()
				t.Fatal("restart admitted task")
			}
		})
	}
}

func TestBreakerReadOnlyCoverageDeclarationValidation(t *testing.T) {
	for _, coverage := range []string{"", fixtureReadOnlyCoverage, " ", strings.Repeat("x", 1025)} {
		e, _ := newSmartBreakerFixture(t, 100)
		registerBreakerFixtureStages(e, "existing")
		err := e.AddStage("detail", StageConfig{Site: "synthetic", WorkerCount: 1, QueueSize: 1, BreakerReadOnlyCoverage: coverage}, nil, nil)
		invalid := coverage == " " || len(coverage) > 1024
		if (err != nil) != invalid {
			t.Fatalf("coverage validation mismatch: length=%d err=%v", len(coverage), err)
		}
	}
}
