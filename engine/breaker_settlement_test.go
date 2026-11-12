package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"gorm.io/gorm"
)

func TestBreakerSettlementRestartAndLostAcknowledgementMySQL(t *testing.T) {
	for _, mode := range []string{"write-failure", "lost-ack", "timing-failure"} {
		lostAck := mode == "lost-ack"
		t.Run(mode, func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
				t.Fatal(err)
			}
			e := makeEngine()
			e.breakerStore, e.breakerRuntime = breakerstore.New(db), true
			cfg := breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{FastThreshold: 10, MixedThreshold: 3}}
			e.SetSiteBreaker("synthetic", cfg)
			rows := []models.CrawlerTask{}
			for i := 0; i < 3; i++ {
				rows = append(rows, models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/%d", i), Status: models.TaskStatusPending, Reprocess: 1})
			}
			if err := db.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				smartFailure(e, int(row.ID), "detail", fmt.Sprint(row.ID))
			}
			businessErr := core.WithFailureCode(errors.Join(context.DeadlineExceeded, errors.New("fixture real parse failure")), "parse", "missing-node")
			e.stages["synthetic:detail"].fetcher = &serialErrorFetcher{recordingFetcher: recordingFetcher{stage: "detail"}, result: businessErr}
			var unavailable atomic.Bool
			unavailable.Store(true)
			cb := db.Callback().Update().Before("gorm:update")
			if lostAck {
				cb = db.Callback().Update().After("gorm:commit_or_rollback_transaction")
			}
			if err := cb.Register("test:settlement-outage", func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if ok && (values["status"] == models.TaskStatusFailed || (mode == "timing-failure" && values["last_finished_at"] != nil)) && unavailable.Load() {
					_ = tx.AddError(errors.New("fixture terminal storage unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			e.breakerMu.Lock()
			s := e.smartBreakers["synthetic"]
			e.selectBreakerSampleLocked(s, "detail", fmt.Sprintf("task:%d", rows[0].ID), time.Now())
			s.event.SampleDeadline = time.Now().Add(50 * time.Millisecond)
			deadline := s.event.SampleDeadline
			e.breakerMu.Unlock()
			flushSmart(t, e)
			if err := e.stageHandler(e.stages["synthetic:detail"])(context.Background(), e.taskFromRecord(&rows[0])); err == nil {
				t.Fatal("lost business error")
			}
			flushSmart(t, e)
			if smartLifecycle(e).WindowFailures != 3 {
				t.Fatal("counted unconfirmed terminal")
			}
			if !e.breakerSettlementBlocked("synthetic") {
				t.Fatal("unsettled failure released admission")
			}
			if e.clearSite("synthetic", true) {
				t.Fatal("force resume bypassed unsettled failure")
			}
			if release, ok := e.admitBreakerTask(e.taskFromRecord(&rows[1])); ok {
				release()
				t.Fatal("next sample admitted despite unresolved failure")
			}
			// No hourly reset or explicit recovery may discard the pending result.
			e.tickBreakers(context.Background(), time.Now().Add(2*time.Hour))
			e.breakerMu.Lock()
			retried := e.retryBreakerLocked(s)
			e.breakerMu.Unlock()
			if retried {
				t.Fatal("manual retry discarded pending failure")
			}

			restarted := makeEngine()
			restarted.SetSiteBreaker("synthetic", cfg)
			restarted.breakerStore = breakerstore.New(db)
			ctx, cancel := context.WithCancel(context.Background())
			if err := restarted.InitBreakerRuntime(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()
			restarted.cancel()
			restarted.tickerWG.Wait()
			if restarted.requeueRecoverTask(&rows[0]) {
				t.Fatal("recover queue reran failed business")
			}
			restarted.breakerMu.Lock()
			rs := restarted.smartBreakers["synthetic"]
			p := rs.event.PendingFailure
			if p == nil || p.Fingerprint != incident.ClassifyError(businessErr).Fingerprint || !p.SampleDeadline.Equal(deadline) {
				t.Fatal("restart lost original result or deadline")
			}
			restarted.breakerMu.Unlock()
			// A failed retry is durable and remains blocked.
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			restarted.settleBreakerFailures(retryCtx)
			retryCancel()
			flushSmart(t, restarted)
			if !restarted.breakerSettlementBlocked("synthetic") {
				t.Fatal("failed settlement unblocked site")
			}
			unavailable.Store(false)
			restarted.breakerMu.Lock()
			rs.event.PendingFailure.LastAttempt = time.Time{}
			restarted.breakerMu.Unlock()
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); restarted.settleBreakerFailures(context.Background()) }()
			}
			wg.Wait()
			if !restarted.breakerSettlementBlocked("synthetic") {
				t.Fatal("count checkpoint was not saved yet")
			}
			if restarted.requeueFailedTaskIn(&rows[0]) || restarted.requeueRepeatTaskIn(&rows[0]) {
				t.Fatal("queue replaced unacknowledged checkpoint")
			}
			flushSmart(t, restarted)
			restarted.settleBreakerFailures(context.Background())
			if err := db.First(&rows[0], rows[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			if rows[0].Status != models.TaskStatusFailed || strings.Count(rows[0].Error, "fixture real parse failure") != 1 || rows[0].Reprocess != 1 || rows[0].CompensationVersion != 1 {
				t.Fatalf("terminal replay/duplicate append: %+v", rows[0])
			}
			l := smartLifecycle(restarted)
			if l.WindowFailures != 4 || l.Checks[0].Verified || l.Checks[0].TaskSuccesses != 0 {
				t.Fatalf("bad count/proof: %+v", l)
			}
			var history []models.BreakerHistory
			if err := db.Order("sequence").Find(&history).Error; err != nil {
				t.Fatal(err)
			}
			text := ""
			for _, h := range history {
				text += string(h.Payload)
			}
			for _, action := range []string{"failure_pending", "failure_settlement_restart", "failure_settlement_retry", "failure_settlement_wait", "failure_settled", "fixture real parse failure"} {
				if !strings.Contains(text, action) {
					t.Fatalf("history missing %s", action)
				}
			}
			settled := 0
			for _, h := range history {
				var tr incident.Transition
				if err := json.Unmarshal(h.Payload, &tr); err != nil {
					t.Fatal(err)
				}
				if tr.Action == "failure_settled" {
					settled++
				}
			}
			if settled != 1 {
				t.Fatalf("settlement history count=%d", settled)
			}
			// A second restart sees the same failure count, with no result to replay.
			again := makeEngine()
			again.SetSiteBreaker("synthetic", cfg)
			again.breakerStore = breakerstore.New(db)
			ctx2, cancel2 := context.WithCancel(context.Background())
			if err := again.InitBreakerRuntime(ctx2); err != nil {
				cancel2()
				t.Fatal(err)
			}
			cancel2()
			again.cancel()
			again.tickerWG.Wait()
			if again.breakerSum("synthetic") != 4 || again.smartBreakers["synthetic"].event.PendingFailure != nil {
				t.Fatal("restart recounted or lost settlement")
			}
		})
	}
}

func TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint("round-conflict-", conflict), func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			e := makeEngine()
			repo := &memoryBreakerRepository{}
			e.breakerStore, e.breakerRuntime = repo, true
			cfg := breaker.Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: breaker.Policy{MixedThreshold: 1}}
			e.SetSiteBreaker("synthetic", cfg)
			row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/settlement", Status: models.TaskStatusProcessing, Repeat: 2, Reprocess: 1}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			smartFailure(e, int(row.ID), "detail", "initial")
			flushSmart(t, e)
			task := e.taskFromRecord(&row)
			task.breakerRound = "2:1"
			task.breakerSampleDeadline = time.Now().Add(-time.Second)
			cause := core.RetryAfter(core.WithFailureCode(errors.New("fixture business failure"), "parse", "missing"), time.Now().Add(time.Hour))
			repo.err = errors.New("fixture checkpoint unavailable")
			if err := e.persistTerminal(context.Background(), task, taskRunVersion{2, 1}, models.TaskStatusFailed, cause); err == nil {
				t.Fatal("intent write failure ignored")
			}
			if err := db.First(&row, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if row.Status != models.TaskStatusProcessing || !e.breakerSettlementBlocked("synthetic") {
				t.Fatal("terminal written before durable intent")
			}
			repo.err = nil
			flushSmart(t, e)
			if conflict {
				if err := db.Model(&row).Update("reprocess", 2).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := e.persistTerminal(context.Background(), task, taskRunVersion{2, 1}, models.TaskStatusFailed, cause); err != nil {
					t.Fatal(err)
				}
				e.recordSmartOutcome(task, cause)
				if smartLifecycle(e).WindowFailures != 2 {
					t.Fatal("confirmed result not counted")
				}
				repo.err = errors.New("fixture count checkpoint unavailable")
				if err := e.flushBreakerReports(context.Background()); err == nil {
					t.Fatal("checkpoint outage ignored")
				}
				repo.err = nil
			}
			restarted := makeEngine()
			restarted.SetSiteBreaker("synthetic", cfg)
			restarted.breakerStore = repo
			ctx, cancel := context.WithCancel(context.Background())
			if err := restarted.InitBreakerRuntime(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()
			restarted.cancel()
			restarted.tickerWG.Wait()
			restarted.settleBreakerFailures(context.Background())
			flushSmart(t, restarted)
			if conflict {
				if !restarted.breakerSettlementBlocked("synthetic") || smartLifecycle(restarted).WindowFailures != 1 {
					t.Fatal("newer round mistaken for confirmation")
				}
				if !strings.Contains(smartLifecycle(restarted).Text(), "failure_settlement_wait") {
					t.Fatal("missing conflict report")
				}
			} else {
				if smartLifecycle(restarted).WindowFailures != 2 || restarted.smartBreakers["synthetic"].event.PendingFailure != nil {
					t.Fatal("count lost or duplicated after checkpoint outage")
				}
				if err := db.First(&row, row.ID).Error; err != nil {
					t.Fatal(err)
				}
				if strings.Count(row.Error, "fixture business failure") != 1 || row.CompensationMode != string(core.CompensationRetryAfter) || row.NextCompensationAt == nil || row.CompensationVersion != 1 {
					t.Fatal("lost policy or duplicated terminal update")
				}
			}
		})
	}
}

func TestBreakerSettlementExpiredResultStillReportsConfirmation(t *testing.T) {
	e := mixedSampleFixture(t, 3)
	e.breakerMu.Lock()
	s := e.smartBreakers["synthetic"]
	s.event.PendingFailure = &breaker.PendingFailure{TaskID: 1, Stage: "detail", Repeat: 0, Reprocess: 1, At: time.Now().Add(-2 * time.Hour), Error: "fixture expired failure", Category: "parse", Fingerprint: "old"}
	s.event.Checks[0].Detail = "真实失败待落库确认"
	e.breakerMu.Unlock()
	e.recordSmartOutcome(&Task{ID: 1, Site: "synthetic", Stage: "detail", breakerRound: "0:1"}, errors.New("fixture expired failure"))
	l := smartLifecycle(e)
	if l.WindowFailures != 3 || l.Checks[0].Verified || !strings.Contains(l.Checks[0].Detail, "实际失败已落库") || !strings.Contains(l.Text(), "fixture expired failure") {
		t.Fatalf("expired settlement report inaccurate: %+v", l)
	}
	e.breakerMu.Lock()
	pending := s.event.PendingFailure != nil
	barrier := s.savedRevision < s.controlRevision
	e.breakerMu.Unlock()
	if pending || !barrier {
		t.Fatal("expired result did not settle behind persistence barrier")
	}
}
