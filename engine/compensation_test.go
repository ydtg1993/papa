package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"gorm.io/gorm"
)

func TestCompensationClassificationPreservesNoRetryCompatibility(t *testing.T) {
	for _, tt := range []struct {
		err  error
		mode core.CompensationMode
		kind string
	}{
		{core.WrapNoRetryKind("structure", errors.New("bad document")), core.CompensationAutomatic, "structure"},
		{account.ErrNeedsVerify, core.CompensationAccountReady, "account_needs_verify"},
		{account.ErrExpired, core.CompensationAccountReady, "account_expired"},
		{core.ManualCompensation(account.ErrExpired, "business intervention"), core.CompensationManual, ""},
		{core.PermanentFailure(errors.New("gone"), "deleted"), core.CompensationPermanent, ""},
	} {
		kind, policy := failureCompensation(tt.err)
		if policy.Mode != tt.mode || tt.kind != "" && kind != tt.kind {
			t.Fatalf("%v: mode=%s kind=%s", tt.err, policy.Mode, kind)
		}
	}
}

func TestExplicitAccountFailureRequiresPoolOrBusinessPolicy(t *testing.T) {
	e := &Engine{}
	kind, policy := failureCompensation(account.ErrNeedsVerify)
	row := &models.CrawlerTask{FailureKind: kind, CompensationMode: string(policy.Mode)}
	if reason := e.compensationBlockedReason(row, time.Now()); reason != "account-ready compensation requires task pool ownership" {
		t.Fatalf("missing dependency was silently retried: %q", reason)
	}
	_, policy = failureCompensation(core.RetryAfter(account.ErrNeedsVerify, time.Now().Add(-time.Minute)))
	row.CompensationMode = string(policy.Mode)
	row.NextCompensationAt = &policy.NotBefore
	if reason := e.compensationBlockedReason(row, time.Now()); reason != "" {
		t.Fatalf("explicit business recovery policy ignored: %q", reason)
	}
}

func TestCompensationConditionsBlockEveryRequeueWithoutSpendingGeneration(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name, mode, pool string
		at               *time.Time
	}{
		{"manual", "manual", "", nil}, {"permanent", "permanent", "", nil}, {"future", "retry_after", "", &future}, {"unknown", "bad-mode", "", nil}, {"pool-unready", "account_ready", "missing", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(models.TaskStatusFailed)
			f.row["compensation_mode"] = tt.mode
			f.row["account_pool"] = tt.pool
			if tt.at != nil {
				f.row["next_compensation_at"] = *tt.at
			}
			e, _ := urgentEngine(t, f)
			row, err := e.loadTask(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			if e.requeueFailedTaskIn(row) || e.requeueRepeatTaskIn(row) {
				t.Fatal("automatic path bypassed condition")
			}
			if err = e.RetryTaskVersioned(context.Background(), 7, 0, 0); !errors.Is(err, ErrCompensationBlocked) {
				t.Fatalf("manual path bypassed condition: %v", err)
			}
			if tt.pool == "" && e.requeueRecoverTask(row) {
				t.Fatal("startup path bypassed durable condition")
			}
			if f.written() != "" {
				t.Fatalf("waiting consumed generation or changed task: %s", f.written())
			}
		})
	}
}

func TestCompensationSQLGuardsCoverQueueClaimAndRecovery(t *testing.T) {
	db := dryRunMySQL(t)
	e := &Engine{db: db, startedAt: time.Now()}
	for name, query := range map[string]*gorm.DB{
		"claim": claimScope(db, 7), "manual": retryScope(db, 7, 4), "repeat": repeatResetScope(db, 7),
		"error":   compensationVersionScope(db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusFailed), &models.CrawlerTask{CompensationVersion: 9}),
		"startup": e.recoverQueueQuery()(),
	} {
		sql := query.Find(&[]models.CrawlerTask{}).Statement.SQL.String()
		for _, part := range []string{"compensation_mode IN", "next_compensation_at IS NULL OR next_compensation_at <= NOW()"} {
			if !strings.Contains(sql, part) {
				t.Fatalf("%s missing %s: %s", name, part, sql)
			}
		}
	}
	res := db.Model(&models.CrawlerTask{}).Where("id = ?", 7).Updates(taskStatusUpdates(models.TaskStatusFailed, core.RetryAfter(errors.New("busy"), time.Now().Add(time.Minute))))
	for _, part := range []string{"`failure_kind`", "`compensation_mode`", "`compensation_version`=compensation_version + 1", "`next_compensation_at`"} {
		if !strings.Contains(res.Statement.SQL.String(), part) {
			t.Fatalf("terminal condition not persisted: %s", res.Statement.SQL.String())
		}
	}
}

func TestSuspiciousFailureStopsImmediateStageRetries(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub", err: account.ErrNeedsVerify}
	info := e.stages["stub:stub"]
	info.fetcher = fetcher
	info.config.MaxAttempts = 3
	if err := e.stageHandler(info)(context.Background(), task); !errors.Is(err, account.ErrNeedsVerify) {
		t.Fatal(err)
	}
	if len(fetcher.calls()) != 1 {
		t.Fatalf("suspicious attempt replayed %d times", len(fetcher.calls()))
	}
	if !strings.Contains(f.writtenArgs(), "account_ready") || !strings.Contains(f.writtenArgs(), "account_needs_verify") {
		t.Fatalf("failure classification not durable: %s", f.writtenArgs())
	}
}

type businessCompensationOnlyError struct{}

func (businessCompensationOnlyError) Error() string { return "synthetic manual review" }
func (businessCompensationOnlyError) Compensation() core.CompensationPolicy {
	return core.CompensationPolicy{Mode: core.CompensationManual, Reason: "review"}
}

func TestBusinessCompensationInterfaceStopsImmediateStageRetries(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub", err: businessCompensationOnlyError{}}
	info := e.stages["stub:stub"]
	info.fetcher = fetcher
	info.config.MaxAttempts = 3
	if err := e.stageHandler(info)(context.Background(), task); err == nil {
		t.Fatal("missing failure")
	}
	if len(fetcher.calls()) != 1 || !strings.Contains(f.writtenArgs(), "manual") {
		t.Fatal("business policy retried immediately or was not persisted")
	}
}

func TestAccountAttemptHistoryIsBoundedAndIndependentOfTrace(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	history := make([]AccountAttemptRecord, 8)
	for i := range history {
		history[i] = AccountAttemptRecord{AccountID: "old", Repeat: i + 1, At: time.Now().Add(-time.Hour)}
	}
	raw, _ := json.Marshal(history)
	f.row["account_attempts"] = raw
	ctx := account.WithAttempt(context.Background(), &account.Attempt{Account: account.Key{Site: "stub", ID: "new"}, SelectionVersion: 17, CredentialVersion: 4})
	if err := e.persistAccountAttempt(ctx, task, taskRunVersion{Repeat: 9, Reprocess: 3}); err != nil {
		t.Fatal(err)
	}
	var got []AccountAttemptRecord
	for _, args := range f.args {
		for _, arg := range args {
			if value, ok := arg.(string); ok && strings.HasPrefix(value, "[{\"account_id\"") {
				if err := json.Unmarshal([]byte(value), &got); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if len(got) != 8 || got[0].Repeat != 2 || got[7].AccountID != "new" || got[7].At.IsZero() || got[7].Reprocess != 3 {
		t.Fatalf("history not bounded/complete: %+v", got)
	}
	if !strings.Contains(f.written(), "last_account_id") {
		t.Fatal("latest association not indexed")
	}
}

func TestCompensationPreviewAndBatchRejectChangedVersion(t *testing.T) {
	f := newFakeTaskDB()
	f.row["status"] = int64(models.TaskStatusFailed)
	f.row["compensation_version"] = int64(8)
	f.row["compensation_mode"] = "manual"
	e, _ := urgentEngine(t, f)
	preview, err := e.PreviewCompensation(context.Background(), CompensationFilter{Site: "stub", AccountPool: "p", AccountID: "alice"})
	if err != nil || preview.Blocked != 1 || preview.Eligible != 0 || preview.HistoryLimit != 8 || preview.Items[0].Version != 8 {
		t.Fatalf("bad preview: %+v %v", preview, err)
	}
	queries := strings.Join(f.queries, "\n")
	for _, part := range []string{"site = ?", "account_pool = ?", "last_account_id = ? OR JSON_CONTAINS"} {
		if !strings.Contains(queries, part) {
			t.Fatalf("missing filter %s: %s", part, queries)
		}
	}
	results, err := e.BatchCompensate(context.Background(), []CompensationRequest{{ID: 7, Version: 7}})
	if err != nil || len(results) != 1 || results[0].Submitted || results[0].Error == "" || f.written() != "" {
		t.Fatalf("stale preview replayed: %+v %v", results, err)
	}
}

func TestReleaseCompensationIsCASManualOnlyAndDoesNotEnqueue(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)
	if err := e.ReleaseTaskCompensation(context.Background(), 7, 9); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"compensation_mode = ?", "compensation_version = ?", "status = ?", "compensation_version + 1"} {
		if !strings.Contains(f.written(), part) {
			t.Fatalf("release missing %s: %s", part, f.written())
		}
	}
	if !strings.Contains(f.writtenArgs(), "manual") {
		t.Fatal("release could revive permanent failure")
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("release unexpectedly submitted task")
	}
	if strings.Contains(f.written(), "`reprocess`=") {
		t.Fatal("release spent reprocess generation")
	}
}

func TestAccountHealthNotificationsThrottleAndDeliverRecovery(t *testing.T) {
	e := &Engine{}
	n := &recordingNotifier{}
	e.AddNotifier(n)
	event := account.HealthEvent{Key: account.Key{Site: "s", ID: "a"}, Pool: account.PoolKey{Site: "s", Name: "p"}, Kind: "pool-exhausted", Reason: "no-ready-account"}
	e.NotifyAccountHealthEvent(event)
	e.NotifyAccountHealthEvent(event)
	event.Kind = "recovered"
	e.NotifyAccountHealthEvent(event)
	got := n.snapshot()
	if len(got) != 2 || got[0].Level != core.AlertCritical || got[1].Level != core.AlertInfo || got[0].AccountPool != "p" {
		t.Fatalf("bad aggregate notification behavior: %+v", got)
	}
}
