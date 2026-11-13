package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"gorm.io/gorm"
)

func TestBreakerRecoveryAdmissionIsSerial(t *testing.T) {
	e := mixedSampleFixture(t, 3)
	e.breakers.breakerMu.Lock()
	e.breakers.breakerMu.Unlock()
	a, exitA := admitSmartTrial(t, e, 1, "detail")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if done, ok := e.breakers.admitBreakerTask(&Task{ID: id, Site: "synthetic", Stage: "detail"}); ok {
				done()
				t.Error("parallel admission during selected sample")
			}
		}(i%3 + 1)
	}
	wg.Wait()
	a.breakerRound = "verified-A"
	e.recordSmartOutcome(a, nil)
	exitA()
	if done, ok := e.breakers.admitBreakerTask(&Task{ID: 1, Site: "synthetic", Stage: "detail"}); ok {
		done()
		t.Fatal("completed sample executed again")
	}
	verificationSuccess(t, e, 2, "verified-B")
	verificationSuccess(t, e, 3, "verified-C")
	if !smartLifecycle(e).Checks[0].Verified {
		t.Fatal("serial verification did not complete")
	}
}

type serialDeadlineFetcher struct {
	recordingFetcher
	entered, cancelled, exit chan struct{}
}

func (f *serialDeadlineFetcher) FetchHandler(ctx context.Context, _ *Task, _ *Engine) error {
	close(f.entered)
	<-ctx.Done()
	close(f.cancelled)
	<-f.exit // deliberately ignores cancellation until the test permits exit
	return nil
}

func TestBreakerSerialDeadlineCancelsRealHandlerButWaitsForExitMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.breakers.breakerStore, e.breakers.breakerRuntime = &memoryBreakerRepository{}, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Minute, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 1}})
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/timeout", Status: models.TaskStatusPending, Reprocess: 1}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	smartFailure(e, int(row.ID), "detail", "first")
	f := &serialDeadlineFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, entered: make(chan struct{}), cancelled: make(chan struct{}), exit: make(chan struct{})}
	var exitOnce sync.Once
	exit := func() { exitOnce.Do(func() { close(f.exit) }) }
	defer exit()
	info := e.stages["synthetic:detail"]
	info.fetcher = f
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	e.breakers.selectBreakerSampleLocked(s, "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
	s.event.SampleDeadline = time.Now().Add(200 * time.Millisecond)
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	done := make(chan error, 1)
	go func() { done <- e.stageHandler(info)(context.Background(), e.taskFromRecord(&row)) }()
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not admitted")
	}
	select {
	case <-f.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive sample deadline")
	}
	e.tickBreakers(context.Background(), time.Now())
	flushSmart(t, e)
	if release, ok := e.breakers.admitBreakerTask(&Task{ID: 99, Site: "synthetic", Stage: "detail"}); ok {
		release()
		t.Fatal("uncooperative handler overlapped next sample")
	}
	exit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler exit did not settle")
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusSuccess || row.LastFinishedAt == nil {
		t.Fatal("late completed business lost its terminal state")
	}
	l := smartLifecycle(e)
	if l.Checks[0].TaskSuccesses != 0 || l.Checks[0].Verified || l.WindowSuccesses != 1 || !strings.Contains(l.Text(), "sample_late_result") || !strings.Contains(l.Text(), "sample_timeout") || !strings.Contains(l.Text(), "sample_exited") {
		t.Fatal("late business completion certified health or lost history")
	}
}

func TestBreakerSerialDeadlineIncludesResourceWaitMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.breakers.breakerStore, e.breakers.breakerRuntime = &memoryBreakerRepository{}, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 1}})
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/wait", Status: models.TaskStatusPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	smartFailure(e, int(row.ID), "detail", "first")
	cfg := governance.DefaultConfig()
	cfg.Resources.Enabled = true
	cfg.Resources.TaskMax = 1
	g, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(g, nil)
	held, err := g.Acquire(context.Background(), "other", governance.ClassTask)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	f := &recordingFetcher{stage: "detail"}
	e.stages["synthetic:detail"].fetcher = f
	e.stages["synthetic:detail"].config.WorkerCount = 1
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	e.breakers.selectBreakerSampleLocked(s, "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
	s.event.SampleDeadline = time.Now().Add(150 * time.Millisecond)
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	err = e.stageHandler(e.stages["synthetic:detail"])(context.Background(), e.taskFromRecord(&row))
	if !errors.Is(err, context.DeadlineExceeded) || len(f.calls()) != 0 {
		t.Fatalf("resource wait escaped deadline: %v", err)
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusPending || smartLifecycle(e).Checks[0].Verified {
		t.Fatal("waiting task claimed or certified")
	}
}

func TestBreakerSerialWaitingTimeoutChecksRemainingSamplesWithoutRecovery(t *testing.T) {
	e := mixedSampleFixture(t, 3)
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	s.event.Deadline = time.Now().Add(time.Hour)
	e.breakers.selectBreakerSampleLocked(s, "detail", "task:1", time.Now())
	deadline := s.event.SampleDeadline
	e.breakers.breakerMu.Unlock()
	if d := time.Until(deadline); d < 9*time.Minute || d > 10*time.Minute {
		t.Fatalf("wrong sample budget %v", d)
	}
	flushSmart(t, e)
	e.tickBreakers(context.Background(), deadline.Add(time.Millisecond))
	verificationSuccess(t, e, 2, "B")
	verificationSuccess(t, e, 3, "C")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	l := expectSmartState(t, e, "manual")
	if l.Checks[0].Verified || len(l.Checks[0].TimedOutIDs) != 1 || l.Checks[0].TaskSuccesses != 2 || !strings.Contains(l.Text(), "sample_timeout") {
		t.Fatalf("timeout was hidden or replaced by B/C: %+v", l.Checks)
	}
}

func TestBreakerSerialUnfinishedTimeoutBlocksNextAndHourlyRetry(t *testing.T) {
	e := mixedSampleFixture(t, 3)
	a, exitA := admitSmartTrial(t, e, 1, "detail")
	defer exitA()
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	s.event.SampleDeadline = time.Now().Add(-time.Second)
	a.breakerSampleDeadline = s.event.SampleDeadline
	e.breakers.breakerMu.Unlock()
	e.tickBreakers(context.Background(), time.Now())
	flushSmart(t, e)
	if done, ok := e.breakers.admitBreakerTask(&Task{ID: 2, Site: "synthetic", Stage: "detail"}); ok {
		done()
		t.Fatal("timeout released running A")
	}
	e.breakers.breakerMu.Lock()
	s.event.Deadline = time.Now().Add(-time.Second)
	e.breakers.breakerMu.Unlock()
	e.tickBreakers(context.Background(), time.Now())
	l := expectSmartState(t, e, "manual")
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.RecheckAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "manual")
	if !strings.Contains(l.Text(), "hourly_recheck_skipped") {
		t.Fatal("missing occupied hourly report")
	}
	e.recordSmartOutcome(a, nil)
	if smartLifecycle(e).Checks[0].TaskSuccesses != 0 {
		t.Fatal("late success counted")
	}
	exitA()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), l.RecheckAt.Add(time.Millisecond))
	l = expectSmartState(t, e, "cooldown")
	if l.Checks[0].Verified || len(l.Checks[0].TimedOutIDs) != 0 || l.Cycle != 2 {
		t.Fatal("new cycle reused old proof or timeout")
	}
}

func TestBreakerSerialRestartPreservesDeadlineAndRejectsExpiredSelection(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			e := mixedSampleFixture(t, 3)
			e.breakers.breakerMu.Lock()
			s := e.breakers.smartBreakers["synthetic"]
			e.breakers.selectBreakerSampleLocked(s, "detail", "task:1", time.Now())
			if expired {
				s.event.SampleDeadline = time.Now().Add(-time.Second)
			}
			deadline := s.event.SampleDeadline
			e.breakers.breakerMu.Unlock()
			flushSmart(t, e)
			restarted, _ := newSmartBreakerFixture(t, 100)
			restarted.breakers.breakerStore, restarted.breakers.breakerRuntime = e.breakers.breakerStore, false
			registerBreakerFixtureStages(restarted, "detail")
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); restarted.tickerWG.Wait() }()
			if err := restarted.InitBreakerRuntime(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
			restarted.tickerWG.Wait()
			l := smartLifecycle(restarted)
			if l.Checks[0].Verified || l.Checks[0].TaskSuccesses != 0 {
				t.Fatal("reused old proof")
			}
			restarted.breakers.breakerMu.Lock()
			kept := restarted.breakers.smartBreakers["synthetic"].event.SampleDeadline
			restarted.breakers.breakerMu.Unlock()
			if expired {
				if l.State != "manual" || len(l.Checks[0].TimedOutIDs) != 1 {
					t.Fatal("expired selection restarted as healthy")
				}
			} else if !kept.Equal(deadline) {
				t.Fatal("restart extended selected sample deadline")
			}
			if !strings.Contains(l.Text(), "sample_interrupted") {
				t.Fatal("missing restart sample history")
			}
		})
	}
}

func TestBreakerRejectsUnsupportedSnapshotBeforeRestore(t *testing.T) {
	for _, mode := range []string{"version", "window"} {
		t.Run(mode, func(t *testing.T) {
			e := mixedSampleFixture(t, 3)
			repo := e.breakers.breakerStore.(*memoryBreakerRepository)
			repo.mu.Lock()
			for id, row := range repo.rows {
				if mode == "version" {
					row.Control.Version = 0
				} else {
					row.Window = nil
				}
				repo.rows[id] = row
			}
			repo.mu.Unlock()
			restarted, _ := newSmartBreakerFixture(t, 100)
			restarted.breakers.breakerStore, restarted.breakers.breakerRuntime = repo, false
			registerBreakerFixtureStages(restarted, "detail")
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); restarted.tickerWG.Wait() }()
			if err := restarted.InitBreakerRuntime(ctx); err == nil {
				t.Fatal("unsupported snapshot restored")
			}
		})
	}
}

func TestBreakerSerialRequeueExcludesNormalAdmissionMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.breakers.breakerStore = &memoryBreakerRepository{}
	e.breakers.breakerRuntime = true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Minute, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 3}})
	for i := 1; i <= 3; i++ {
		row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/%d", i), Status: models.TaskStatusFailed}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		smartFailure(e, int(row.ID), "detail", fmt.Sprint(i))
	}
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	flushSmart(t, e)
	interleaved := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:serial-admission", func(tx *gorm.DB) {
		if interleaved || tx.Statement.Table != "crawler_tasks" {
			return
		}
		interleaved = true
		if done, ok := e.breakers.admitBreakerTask(&Task{ID: 1, Site: "synthetic", Stage: "detail"}); ok {
			done()
			t.Error("normal scheduling entered during sample lookup")
		}
	}); err != nil {
		t.Fatal(err)
	}
	e.retryBreakerSamples(context.Background())
	if !interleaved {
		t.Fatal("SQL interleaving not reached")
	}
	var row models.CrawlerTask
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatal(err)
	}
	if row.Reprocess != 1 || row.Status != models.TaskStatusPending {
		t.Fatal("selected row was not requeued once")
	}
	// Its real completion is recorded before any next selection. The same ID
	// cannot be requeued again merely because another scheduler won execution.
	if err := db.Model(&row).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	verificationSuccess(t, e, 1, "0:1")
	e.breakers.breakerMu.Lock()
	e.breakers.smartBreakers["synthetic"].nextSampleRetry = time.Time{}
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatal(err)
	}
	if row.Reprocess != 1 || row.Status != models.TaskStatusSuccess {
		t.Fatal("completed sample unnecessarily requeued")
	}
}

func TestBreakerSerialDelayedClaimCannotRenewDeadlineMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.breakers.breakerStore, e.breakers.breakerRuntime = &memoryBreakerRepository{}, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 3, MaxRecovery: time.Hour}})
	var rows []models.CrawlerTask
	for i := 1; i <= 3; i++ {
		row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/wait-%d", i), Status: models.TaskStatusFailed}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
		smartFailure(e, int(row.ID), "detail", fmt.Sprint(i))
	}
	later := time.Now().Add(time.Hour)
	if err := db.Model(&rows[0]).Updates(map[string]any{"status": models.TaskStatusPending, "not_before": later}).Error; err != nil {
		t.Fatal(err)
	}
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	flushSmart(t, e)
	e.breakers.breakerMu.Lock()
	deadline := e.breakers.smartBreakers["synthetic"].event.SampleDeadline
	e.breakers.breakerMu.Unlock()
	for i := 0; i < 3; i++ {
		e.breakers.breakerMu.Lock()
		e.breakers.smartBreakers["synthetic"].nextAdmission = time.Time{}
		e.breakers.breakerMu.Unlock()
		// An early queued copy reaches the admission gate, but DB time rejects its claim.
		if err := e.stageHandler(e.stages["synthetic:detail"])(context.Background(), e.taskFromRecord(&rows[0])); err != nil {
			t.Fatal(err)
		}
		e.breakers.breakerMu.Lock()
		same := e.breakers.smartBreakers["synthetic"].event.SampleDeadline.Equal(deadline)
		e.breakers.breakerMu.Unlock()
		if !same {
			t.Fatal("unsuccessful claim renewed selected sample timeout")
		}
	}
	e.tickBreakers(context.Background(), deadline.Add(time.Millisecond))
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	flushSmart(t, e)
	e.retryBreakerSamples(context.Background())
	if err := db.First(&rows[1], rows[1].ID).Error; err != nil {
		t.Fatal(err)
	}
	if rows[1].Status != models.TaskStatusPending {
		t.Fatal("timed-out pending A still blocked ready B")
	}
	if smartLifecycle(e).Checks[0].Verified {
		t.Fatal("timeout certified health")
	}
}

func TestBreakerSerialProbeSelectedWaitHasTenMinuteLimit(t *testing.T) {
	e, _ := readOnlyFixture(t, "detail")
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	s.event.Deadline = time.Now().Add(time.Hour)
	e.breakers.transitionBreakerLocked(s, "probing", "fixture", time.Now())
	e.breakers.prepareNextBreakerProbeLocked(s, time.Now().Add(-11*time.Minute))
	s.event.NextProbeAt = time.Now().Add(time.Hour)
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	e.tickBreakers(context.Background(), time.Now())
	l := expectSmartState(t, e, "manual")
	if l.Checks[0].Verified || !strings.Contains(l.Text(), "stage_probe_timeout") {
		t.Fatal("probe wait exceeded bound without a report")
	}
}

type serialErrorFetcher struct {
	recordingFetcher
	result error
}

func (f *serialErrorFetcher) FetchHandler(ctx context.Context, _ *Task, _ *Engine) error {
	<-ctx.Done()
	return f.result
}

func TestBreakerSerialLateErrorClassificationMySQL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		business bool
	}{
		{"cancel", context.Canceled, false},
		{"wrapped-deadline", fmt.Errorf("request: %w", context.DeadlineExceeded), false},
		{"business", errors.New("fixture real parse failure"), true},
		{"joined", errors.Join(context.DeadlineExceeded, errors.New("fixture real parse failure")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			e := makeEngine()
			e.breakers.breakerStore, e.breakers.breakerRuntime = &memoryBreakerRepository{}, true
			e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Minute, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 1}})
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/late-error", Status: models.TaskStatusPending, Reprocess: 1}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			smartFailure(e, int(row.ID), "detail", "initial")
			e.stages["synthetic:detail"].fetcher = &serialErrorFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, result: tc.err}
			checked := false
			if err := db.Callback().Update().Before("gorm:update").Register("test:late-failure-slot", func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if !ok || values["status"] != models.TaskStatusFailed {
					return
				}
				checked = true
				if release, ok := e.breakers.admitBreakerTask(&Task{ID: 99, Site: "synthetic", Stage: "detail"}); ok {
					release()
					t.Error("serial slot released before terminal persistence")
				}
			}); err != nil {
				t.Fatal(err)
			}
			e.breakers.breakerMu.Lock()
			s := e.breakers.smartBreakers["synthetic"]
			e.breakers.selectBreakerSampleLocked(s, "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
			s.event.SampleDeadline = time.Now().Add(100 * time.Millisecond)
			e.breakers.breakerMu.Unlock()
			flushSmart(t, e)
			got := e.stageHandler(e.stages["synthetic:detail"])(context.Background(), e.taskFromRecord(&row))
			if got == nil {
				t.Fatal("error lost")
			}
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			l := smartLifecycle(e)
			wantStatus, wantFailures := models.TaskStatusPending, 1
			if tc.business {
				wantStatus, wantFailures = models.TaskStatusFailed, 2
			}
			if row.Status != wantStatus || l.WindowFailures != wantFailures || checked != tc.business {
				t.Fatalf("wrong terminal/count: status=%d failures=%d checked=%v", row.Status, l.WindowFailures, checked)
			}
			if l.Checks[0].Verified || l.Checks[0].TaskSuccesses != 0 || len(l.Checks[0].TimedOutIDs) != 1 {
				t.Fatal("late failure weakened recovery gate")
			}
			if tc.business && (!strings.Contains(l.Text(), "sample_late_result") || !strings.Contains(l.Text(), "fixture real parse failure")) {
				t.Fatal("obs history lost actual late failure")
			}
			flushSmart(t, e)
			e.tickBreakers(context.Background(), time.Now())
			expectSmartState(t, e, "manual")
		})
	}
}
