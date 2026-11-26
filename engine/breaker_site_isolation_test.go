package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

type siteFailureRepository struct {
	breakerRepository
	save func(context.Context, breakerstore.Snapshot) error
}

func (r *siteFailureRepository) Save(ctx context.Context, snapshot breakerstore.Snapshot) error {
	if r.save != nil {
		if err := r.save(ctx, snapshot); err != nil {
			return err
		}
	}
	return r.breakerRepository.Save(ctx, snapshot)
}

// The fake DB verifies dispatch and journal ordering only. SQL CAS and replay
// semantics are covered separately against an isolated MySQL database.
func siteIsolationFixture(t *testing.T, settlement bool) (*Engine, *fakeTaskDB, *siteFailureRepository) {
	t.Helper()
	f := newFakeTaskDB()
	f.row["status"] = int64(models.TaskStatusFailed)
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
	r := &siteFailureRepository{breakerRepository: &memoryBreakerRepository{}}
	e.breakers.breakerStore, e.breakers.breakerRuntime = r, true
	cfg := breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 1}}
	e.SetSiteBreaker("stub", cfg)
	task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com", breakerRound: "0:0"}
	e.recordSmartOutcome(task, errors.New("initial sample failure"))
	flushSmart(t, e)
	if settlement {
		f.row["status"] = int64(models.TaskStatusProcessing)
		if err := e.retainBreakerFailure(context.Background(), task, taskRunVersion{}, errors.New("pending sample failure")); err != nil {
			t.Fatal(err)
		}
	} else {
		e.retryBreakerSamples(context.Background()) // select only; must save before dispatch
		flushSmart(t, e)
	}
	e.SetSiteBreaker("a-unavailable", cfg)
	e.recordSmartOutcome(&Task{ID: 70, Site: "a-unavailable", Stage: "stub", breakerRound: "0:0"}, errors.New("unavailable site failure"))
	r.save = func(_ context.Context, snapshot breakerstore.Snapshot) error {
		if snapshot.Control.Site == "a-unavailable" {
			return errors.New("fixture site A report unavailable")
		}
		return nil
	}
	return e, f, r
}

func TestBreakerSiteJournalFailureStillBlocksTaskWrites(t *testing.T) {
	for _, settlement := range []bool{true, false} {
		name := "sample_selection"
		if settlement {
			name = "settlement_reservation"
		}
		t.Run(name, func(t *testing.T) {
			e, f, r := siteIsolationFixture(t, settlement)
			r.save = func(_ context.Context, snapshot breakerstore.Snapshot) error {
				if snapshot.Control.Site == "stub" {
					return errors.New("fixture own journal unavailable")
				}
				return nil
			}
			if settlement {
				// The prior intent is durable; the newly reserved retry is not.
				e.settleBreakerFailures(context.Background(), "stub")
			} else {
				e.breakers.breakerMu.Lock()
				e.breakers.selectBreakerSampleLocked(e.breakers.smartBreakers["stub"], "stub", "task:7", time.Now())
				e.breakers.breakerMu.Unlock()
				if err := e.writeBreakerReportBatch(context.Background()); err == nil {
					t.Fatal("own journal failure was not reported")
				}
				e.retryBreakerSamples(context.Background(), "stub")
			}
			if f.written() != "" {
				t.Fatal("task write preceded its own durable journal")
			}
			e.breakers.breakerMu.Lock()
			s := e.breakers.smartBreakers["stub"]
			blocked, reserved := s.savedRevision < s.controlRevision, s.sampleDispatching
			e.breakers.breakerMu.Unlock()
			if !blocked || reserved {
				t.Fatalf("failed journal lost protection or leaked reservation: blocked=%v reserved=%v", blocked, reserved)
			}
		})
	}
}

func TestBreakerSiteWriterSlowSiteHasSeparateBudget(t *testing.T) {
	e, _, r := siteIsolationFixture(t, true)
	var savedB []uint64
	r.save = func(ctx context.Context, snapshot breakerstore.Snapshot) error {
		if snapshot.Control.Site == "a-unavailable" {
			<-ctx.Done()
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("site B inherited site A's expired budget: %v", err)
		}
		savedB = append(savedB, snapshot.Control.Revision)
		// Fresh evidence arriving on every save keeps dirty=true, but cannot
		// starve a decision whose control revision was already acknowledged.
		e.breakers.breakerMu.Lock()
		e.breakers.noteBreakerLocked(e.breakers.smartBreakers["stub"], "fixture_evidence", "stub", "new evidence", time.Now())
		e.breakers.breakerMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		before := len(savedB)
		if err := e.writeBreakerReportBatch(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("missing slow site deadline: %v", err)
		}
		if len(savedB) <= before {
			t.Fatalf("site B did not progress in batch %d", i)
		}
	}
	e.breakers.breakerMu.Lock()
	pending := e.breakers.smartBreakers["stub"].event.PendingFailure
	e.breakers.breakerMu.Unlock()
	if pending != nil {
		t.Fatal("slow site or continuously dirty evidence prevented B settlement")
	}
}

func TestBreakerSiteWriterCancellationStopsRemainingSites(t *testing.T) {
	e, f, r := siteIsolationFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.save = func(_ context.Context, snapshot breakerstore.Snapshot) error {
		if snapshot.Control.Site != "a-unavailable" {
			t.Fatal("writer entered another site after cancellation")
		}
		cancel()
		return ctx.Err()
	}
	if err := e.writeBreakerReportBatch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("missing cancellation: %v", err)
	}
	if f.written() != "" {
		t.Fatal("canceled writer performed a task write")
	}
}

func TestBreakerSiteWriterSettlementAndSampleIsolationMySQL(t *testing.T) {
	for _, settlement := range []bool{true, false} {
		name := "sample"
		if settlement {
			name = "settlement_lost_ack"
		}
		t.Run(name, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
				t.Fatal(err)
			}
			e := makeEngine()
			r := &siteFailureRepository{breakerRepository: breakerstore.New(db)}
			e.breakers.breakerStore, e.breakers.breakerRuntime = r, true
			cfg := breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 1}}
			e.SetSiteBreaker("synthetic", cfg)
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/site-isolation", Status: models.TaskStatusFailed}
			if settlement {
				row.Status = models.TaskStatusProcessing
				row.Reprocess = 1 // the original round failed; this selected retry now needs settlement
			}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			smartFailure(e, int(row.ID), "detail", "initial")
			flushSmart(t, e)
			if settlement {
				if err := e.retainBreakerFailure(context.Background(), e.taskFromRecord(&row), taskRunVersion{Reprocess: row.Reprocess}, errors.New("original isolated failure")); err != nil {
					t.Fatal(err)
				}
			} else {
				e.retryBreakerSamples(context.Background())
				flushSmart(t, e)
			}
			e.SetSiteBreaker("a-unavailable", cfg)
			e.recordSmartOutcome(&Task{ID: 70, Site: "a-unavailable", Stage: "detail", breakerRound: "0:0"}, errors.New("unavailable site"))
			lostJournalAck := settlement
			r.save = func(ctx context.Context, snapshot breakerstore.Snapshot) error {
				if snapshot.Control.Site == "a-unavailable" {
					return errors.New("fixture site A save failure")
				}
				if lostJournalAck {
					lostJournalAck = false
					if err := r.breakerRepository.Save(ctx, snapshot); err != nil {
						return err
					}
					return errors.New("fixture site B lost journal acknowledgement")
				}
				return nil
			}
			terminalWrites := 0
			if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:site-isolation-lost-terminal-ack", func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if ok && values["status"] == models.TaskStatusFailed {
					terminalWrites++
					if terminalWrites == 1 {
						_ = tx.AddError(errors.New("fixture lost terminal acknowledgement"))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			if settlement {
				_ = e.writeBreakerReportBatch(context.Background())
				if terminalWrites != 0 {
					t.Fatal("unacknowledged own journal allowed terminal CAS")
				}
				_ = e.writeBreakerReportBatch(context.Background())
				if terminalWrites != 0 {
					t.Fatal("settlement retry ignored its one-minute backoff")
				}
				e.breakers.breakerMu.Lock()
				e.breakers.smartBreakers["synthetic"].event.PendingFailure.LastAttempt = time.Now().Add(-time.Minute)
				e.breakers.breakerMu.Unlock()
			}
			// A remains unavailable in every batch, including replay after B's
			// journal commit and terminal CAS each lost their acknowledgement.
			batches := 3
			if settlement {
				batches = 1
			}
			for i := 0; i < batches; i++ {
				if err := e.writeBreakerReportBatch(context.Background()); err == nil {
					t.Fatal("site A outage unexpectedly disappeared")
				}
			}
			if settlement {
				// Reconcile again without starting the next legitimate recovery
				// sample: a repeated settlement must not append/increment twice.
				for i := 0; i < 2; i++ {
					if err := e.breakers.flushBreakerReportsForSite(context.Background(), "synthetic"); err != nil {
						t.Fatal(err)
					}
					e.settleBreakerFailures(context.Background(), "synthetic")
				}
			}
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if settlement {
				if row.Status != models.TaskStatusFailed || row.CompensationVersion != 1 || row.Reprocess != 1 || strings.Count(row.Error, "original isolated failure") != 1 || smartLifecycle(e).WindowFailures != 2 {
					t.Fatalf("settlement was lost or replayed: row=%+v failures=%d", row, smartLifecycle(e).WindowFailures)
				}
			} else if row.Status != models.TaskStatusPending || row.Reprocess != 1 || row.CompensationVersion != 1 {
				t.Fatalf("sample not requeued exactly once: %+v", row)
			}
		})
	}
}

func TestBreakerFailureSettlementIgnoresOtherSiteWriteFailure(t *testing.T) {
	e, f, _ := siteIsolationFixture(t, true)
	e.settleBreakerFailures(context.Background())
	if !strings.Contains(f.written(), "UPDATE `crawler_tasks`") {
		t.Fatal("durable site B failure was blocked by site A report error")
	}
	e.breakers.breakerMu.Lock()
	pending := e.breakers.smartBreakers["stub"].event.PendingFailure
	e.breakers.breakerMu.Unlock()
	if pending != nil {
		t.Fatal("site B failure was not reconciled")
	}
}

func TestBreakerRetainFailureOnlyRequiresOwnJournal(t *testing.T) {
	e, _, _ := siteIsolationFixture(t, true)
	if err := e.retainBreakerFailure(context.Background(), &Task{ID: 7, Site: "stub", Stage: "stub"}, taskRunVersion{}, errors.New("pending sample failure")); err != nil {
		t.Fatalf("durable site B journal was rejected because of site A: %v", err)
	}
}

func TestBreakerWriterContinuesOtherSiteSettlementAndSamples(t *testing.T) {
	for _, settlement := range []bool{true, false} {
		name := "sample"
		if settlement {
			name = "settlement"
		}
		t.Run(name, func(t *testing.T) {
			e, f, _ := siteIsolationFixture(t, settlement)
			wrote := make(chan struct{})
			var once sync.Once
			f.onExec = func(int) { once.Do(func() { close(wrote) }) }
			ctx, cancel := context.WithCancel(context.Background())
			e.tickerWG.Add(1)
			go e.writeBreakerReports(ctx)
			t.Cleanup(func() { cancel(); e.tickerWG.Wait() })
			select {
			case <-wrote:
			case <-time.After(3 * time.Second):
				t.Fatal("report writer did not advance durable site B while site A failed")
			}
			cancel()
			e.tickerWG.Wait()
			if count := strings.Count(f.written(), "UPDATE `crawler_tasks`"); count != 1 {
				t.Fatalf("site B task writes=%d, want one", count)
			}
		})
	}
}
