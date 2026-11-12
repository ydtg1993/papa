package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBreakerSettlementOldLiveWindow(t *testing.T) {
	e := mixedSampleFixture(t, 3)
	e.breakers.breakerMu.Lock()
	s := e.breakers.smartBreakers["synthetic"]
	old := time.Now().Add(-2 * time.Hour)
	cp := s.detector.Checkpoint()
	for i := range cp.Failures {
		cp.Failures[i].At = old
	}
	s.detector.Restore(cp, s.epoch, old)
	s.event.PendingFailure = &breaker.PendingFailure{TaskID: 1, Stage: "detail", Reprocess: 1, At: old, Error: "old business failure", Category: "parse", Fingerprint: "old"}
	epoch := s.epoch
	e.breakers.breakerMu.Unlock()
	e.recordSmartOutcome(&Task{ID: 1, Site: "synthetic", Stage: "detail", breakerEpoch: epoch, breakerRound: "0:1"}, errors.New("old business failure"))
	if got := smartLifecycle(e).WindowFailures; got != 0 {
		t.Fatalf("expired failures revived during live settlement: got=%d want=0", got)
	}
}

type reviewImmediateFailure struct{ recordingFetcher }

func (f *reviewImmediateFailure) FetchHandler(context.Context, *Task, *Engine) error {
	return WrapNoRetry(errors.New("fixture known business failure"))
}
func TestBreakerSettlementIntentBeforeTimingRetryMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	repo := &memoryBreakerRepository{}
	e.breakers.breakerStore, e.breakers.breakerRuntime = repo, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{MixedThreshold: 1}})
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/a", Status: models.TaskStatusPending, Reprocess: 1}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	smartFailure(e, int(row.ID), "detail", "initial")
	e.stages["synthetic:detail"].fetcher = &reviewImmediateFailure{recordingFetcher{stage: "detail"}}
	e.breakers.breakerMu.Lock()
	e.breakers.selectBreakerSampleLocked(e.breakers.smartBreakers["synthetic"], "detail", "task:1", time.Now())
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checked := false
	durable := false
	attempts := 0
	if err := db.Callback().Update().Before("gorm:update").Register("review:timing", func(tx *gorm.DB) {
		v, ok := tx.Statement.Dest.(map[string]any)
		if !ok || v["last_finished_at"] == nil || v["status"] != nil {
			return
		}
		attempts++
		if attempts == 2 {
			checked = true
			repo.mu.Lock()
			for _, snap := range repo.rows {
				durable = durable || snap.Control.PendingFailure != nil
			}
			repo.mu.Unlock()
			// Cleanup still owns inflight. A concurrent writer and cancellation
			// cannot settle or reset this known failed round behind its back.
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); e.settleBreakerFailures(context.Background()) }()
			}
			wg.Wait()
			e.releaseClaim(e.taskFromRecord(&row))
			var current models.CrawlerTask
			if queryErr := db.First(&current, row.ID).Error; queryErr != nil {
				t.Error(queryErr)
			} else if current.Status != models.TaskStatusProcessing {
				t.Error("inflight failure was settled or reset before cleanup exited")
			}
			if release, ok := e.breakers.admitBreakerTask(&Task{ID: 99, Site: "synthetic", Stage: "detail"}); ok {
				release()
				t.Error("next task admitted while cleanup is inflight")
			}
			cancel()
		}
		_ = tx.AddError(errors.New("fixture finish time unavailable"))
	}); err != nil {
		t.Fatal(err)
	}
	_ = e.stageHandler(e.stages["synthetic:detail"])(ctx, e.taskFromRecord(&row))
	if !checked {
		t.Fatal("fixture did not reach finished-time write")
	}
	if !durable {
		t.Fatal("known terminal failure not durable before finish-time retry; restart may rerun business")
	}
}

// Hooks surround the real snapshot/history transaction, so commit/ack failures
// exercise durable MySQL state rather than a constructed PendingFailure.
type settlementCutRepository struct {
	breakerRepository
	before func(breakerstore.Snapshot) error
	after  func(breakerstore.Snapshot) error
}

func (r *settlementCutRepository) Save(ctx context.Context, s breakerstore.Snapshot) error {
	if r.before != nil {
		if err := r.before(s); err != nil {
			return err
		}
	}
	if err := r.breakerRepository.Save(ctx, s); err != nil {
		return err
	}
	if r.after != nil {
		return r.after(s)
	}
	return nil
}

type settlementSequenceFetcher struct {
	recordingFetcher
	calls       int
	results     []error
	panicResult bool
}

func (f *settlementSequenceFetcher) FetchHandler(context.Context, *Task, *Engine) error {
	f.calls++
	if f.panicResult {
		panic("fixture panic result")
	}
	return f.results[min(f.calls-1, len(f.results)-1)]
}
func TestBreakerSettlementCrashCutsFromHandlerMySQL(t *testing.T) {
	for _, cut := range []string{"intent-ack-lost", "terminal-ack-lost", "terminal-before-count", "count-before-commit", "count-ack-lost", "complete"} {
		t.Run(cut, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
				t.Fatal(err)
			}
			e := makeEngine()
			collector := &collectorEmitter{}
			e.SetEventsEmitter(collector)
			base := breakerstore.New(db)
			repo := &settlementCutRepository{breakerRepository: base}
			e.breakers.breakerStore, e.breakers.breakerRuntime = repo, true
			cfg := breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{MixedThreshold: 1}}
			e.SetSiteBreaker("synthetic", cfg)
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/cut", Status: models.TaskStatusPending, Reprocess: 1}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			smartFailure(e, int(row.ID), "detail", "initial")
			f := &settlementSequenceFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, results: []error{WrapNoRetry(errors.New("fixture cut business error"))}}
			e.stages["synthetic:detail"].fetcher = f
			e.breakers.breakerMu.Lock()
			e.breakers.selectBreakerSampleLocked(e.breakers.smartBreakers["synthetic"], "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
			e.breakers.breakerMu.Unlock()
			flushSmart(t, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			intentSaved, blocked, cutReached := false, false, false
			repo.before = func(s breakerstore.Snapshot) error {
				if blocked {
					return errors.New("fixture snapshot unavailable after cut")
				}
				if cut == "count-before-commit" && intentSaved && s.Control.PendingFailure == nil {
					blocked = true
					cutReached = true
					return errors.New("fixture count commit rejected")
				}
				return nil
			}
			repo.after = func(s breakerstore.Snapshot) error {
				if s.Control.PendingFailure != nil {
					intentSaved = true
				}
				if (cut == "intent-ack-lost" && s.Control.PendingFailure != nil) || (cut == "count-ack-lost" && intentSaved && s.Control.PendingFailure == nil) {
					blocked = true
					cutReached = true
					cancel()
					return errors.New("fixture snapshot committed but acknowledgement lost")
				}
				return nil
			}
			if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:terminal-cut", func(tx *gorm.DB) {
				v, ok := tx.Statement.Dest.(map[string]any)
				if !ok || v["status"] != models.TaskStatusFailed {
					return
				}
				if cut == "terminal-ack-lost" || cut == "terminal-before-count" {
					cutReached = true
					cancel()
					if cut == "terminal-ack-lost" {
						_ = tx.AddError(errors.New("fixture terminal committed but acknowledgement lost"))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.stageHandler(e.stages["synthetic:detail"])(ctx, e.taskFromRecord(&row)); err == nil {
				t.Fatal("handler failure lost")
			}
			if strings.HasPrefix(cut, "count-") {
				if err := e.breakers.flushBreakerReports(context.Background()); err == nil {
					t.Fatal("missing checkpoint failure")
				}
			}
			if cut == "complete" {
				flushSmart(t, e)
				cutReached = true
			}
			if !cutReached || !intentSaved || f.calls != 1 {
				t.Fatalf("cut not exercised: reached=%v intent=%v calls=%d", cutReached, intentSaved, f.calls)
			}
			// No process-local state from e is used by the new Engine. Stop its loops;
			// keep the exact database contents at the selected durable boundary.
			e.cancel()
			e.tickerWG.Wait()
			if err := db.Callback().Update().Remove("test:terminal-cut"); err != nil {
				t.Fatal(err)
			}
			beforeReconcile := countTaskEvents(collector, events.TypeTaskFailed)
			restarted := makeEngine()
			restarted.SetEventsEmitter(collector)
			restarted.SetSiteBreaker("synthetic", cfg)
			restarted.breakers.breakerStore = base
			restoreCtx, restoreCancel := context.WithCancel(context.Background())
			if err := restarted.InitBreakerRuntime(restoreCtx); err != nil {
				restoreCancel()
				t.Fatal(err)
			}
			restoreCancel()
			restarted.cancel()
			restarted.tickerWG.Wait()
			if release, ok := restarted.breakers.admitBreakerTask(&Task{ID: 99, Site: "synthetic", Stage: "detail"}); ok {
				release()
				t.Fatal("restart admitted before revalidation")
			}
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if row.Status == models.TaskStatusProcessing && restarted.requeueRecoverTask(&row) {
				t.Fatal("restart requeued known failed business")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); restarted.settleBreakerFailures(context.Background()) }()
			}
			wg.Wait()
			flushSmart(t, restarted)
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if row.Status != models.TaskStatusFailed || row.Reprocess != 1 || row.CompensationVersion != 1 || strings.Count(row.Error, "fixture cut business error") != 1 {
				t.Fatalf("non-idempotent row: %+v", row)
			}
			if countTaskEvents(collector, events.TypeTaskFailed) != beforeReconcile {
				t.Fatal("reconciliation fabricated another execution failure event")
			}
			if f.calls != 1 || smartLifecycle(restarted).WindowFailures != 2 || smartLifecycle(restarted).Checks[0].Verified {
				t.Fatal("replayed business, wrong count, or reused proof")
			}
			var history []models.BreakerHistory
			if err := db.Find(&history).Error; err != nil {
				t.Fatal(err)
			}
			pending, settled := 0, 0
			for _, h := range history {
				raw := string(h.Payload)
				if strings.Contains(raw, "failure_pending") {
					pending++
				}
				if strings.Contains(raw, "failure_settled") {
					settled++
				}
			}
			if pending != 1 || settled != 1 {
				t.Fatalf("history lost/duplicated: pending=%d settled=%d", pending, settled)
			}
		})
	}
}

func TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL(t *testing.T) {
	for _, mode := range []string{"retry-success", "exhausted", "panic", "cancel-only", "auxiliary-panic"} {
		t.Run(mode, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			e := makeEngine()
			repo := &memoryBreakerRepository{}
			e.breakers.breakerStore, e.breakers.breakerRuntime = repo, true
			e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{MixedThreshold: 1}})
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/attempt", Status: models.TaskStatusPending, Reprocess: 1}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			smartFailure(e, int(row.ID), "detail", "initial")
			f := &settlementSequenceFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, results: []error{errors.New("fixture retry error"), nil}, panicResult: mode == "panic"}
			if mode == "exhausted" {
				f.results = []error{errors.New("fixture retry error")}
			}
			if mode == "auxiliary-panic" {
				f.results = []error{WrapNoRetry(errors.New("fixture original business error"))}
			}
			info := e.stages["synthetic:detail"]
			info.fetcher = f
			info.config.MaxAttempts = 2
			if mode == "cancel-only" {
				info.fetcher = &serialErrorFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, result: fmt.Errorf("wrapped: %w", context.Canceled)}
			}
			e.breakers.breakerMu.Lock()
			s := e.breakers.smartBreakers["synthetic"]
			e.breakers.selectBreakerSampleLocked(s, "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
			if mode == "cancel-only" {
				s.event.SampleDeadline = time.Now().Add(50 * time.Millisecond)
			}
			e.breakers.breakerMu.Unlock()
			flushSmart(t, e)
			checks := 0
			if err := db.Callback().Update().Before("gorm:update").Register("test:attempt-intent", func(tx *gorm.DB) {
				v, ok := tx.Statement.Dest.(map[string]any)
				if !ok || v["last_finished_at"] == nil || v["status"] != nil {
					return
				}
				checks++
				e.breakers.breakerMu.Lock()
				has := s.event.PendingFailure != nil
				e.breakers.breakerMu.Unlock()
				expected := mode == "panic" || mode == "auxiliary-panic" || (mode == "exhausted" && f.calls == 2)
				if has != expected {
					t.Errorf("wrong terminal decision mode=%s calls=%d pending=%v", mode, f.calls, has)
				}
				if mode == "auxiliary-panic" {
					panic("fixture auxiliary panic")
				}
			}); err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if r := recover(); r != nil && mode != "panic" && mode != "auxiliary-panic" {
						t.Errorf("unexpected panic %v", r)
					}
				}()
				_ = e.stageHandler(info)(context.Background(), e.taskFromRecord(&row))
			}()
			if checks == 0 {
				t.Fatal("no finish-time checks")
			}
			flushSmart(t, e)
			l := smartLifecycle(e)
			want := 1
			if mode == "exhausted" || mode == "panic" || mode == "auxiliary-panic" {
				want = 2
			}
			if l.WindowFailures != want {
				t.Fatalf("wrong failures %d want %d", l.WindowFailures, want)
			}
			if mode == "auxiliary-panic" {
				if err := db.First(&row, row.ID).Error; err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(row.Error, "fixture original business error") || strings.Contains(row.Error, "fixture auxiliary panic") {
					t.Fatalf("cleanup replaced original result: %s", row.Error)
				}
				if !strings.Contains(l.Evidence[len(l.Evidence)-1].Error, "fixture original business error") {
					t.Fatal("report replaced original result")
				}
			}
		})
	}
}

func TestBreakerSettlementIntentOutageDoesNotRunAuxiliaryWritesMySQL(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	repo := &memoryBreakerRepository{}
	e.breakers.breakerStore, e.breakers.breakerRuntime = repo, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{MixedThreshold: 1}})
	row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/intent-outage", Status: models.TaskStatusPending, Reprocess: 1}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	smartFailure(e, int(row.ID), "detail", "initial")
	f := &settlementSequenceFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, results: []error{WrapNoRetry(errors.New("fixture intent outage result"))}}
	e.stages["synthetic:detail"].fetcher = f
	e.breakers.breakerMu.Lock()
	e.breakers.selectBreakerSampleLocked(e.breakers.smartBreakers["synthetic"], "detail", fmt.Sprintf("task:%d", row.ID), time.Now())
	e.breakers.breakerMu.Unlock()
	flushSmart(t, e)
	repo.err = errors.New("fixture intent unavailable")
	if err := db.Callback().Update().Before("gorm:update").Register("test:no-aux-before-intent", func(tx *gorm.DB) {
		v, ok := tx.Statement.Dest.(map[string]any)
		if ok && (v["last_finished_at"] != nil || v["status"] == models.TaskStatusFailed) {
			t.Error("auxiliary/terminal write preceded durable intent")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.stageHandler(e.stages["synthetic:detail"])(context.Background(), e.taskFromRecord(&row)); err == nil {
		t.Fatal("intent outage ignored")
	}
	if !e.breakers.breakerSettlementBlocked("synthetic") || f.calls != 1 {
		t.Fatal("intent outage replayed or released")
	}
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusProcessing {
		t.Fatal("unconfirmed failure became pending/terminal")
	}
	if err := db.Callback().Update().Remove("test:no-aux-before-intent"); err != nil {
		t.Fatal(err)
	}
	repo.err = nil
	flushSmart(t, e)
	e.settleBreakerFailures(context.Background())
	flushSmart(t, e)
	if smartLifecycle(e).WindowFailures != 2 || f.calls != 1 {
		t.Fatal("resumed settlement lost count or reran business")
	}
}
