package rulestore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAdminPlanAndSQLBounds(t *testing.T) {
	good := RulePlan{Kind: "publish", Site: "site", Generation: "9007199254740993", Bindings: map[string]Reference{"list": {ID: "r", Version: "1"}}}
	if n, err := good.Validate(); err != nil || n != 9007199254740993 {
		t.Fatalf("generation: %d %v", n, err)
	}
	for _, g := range []string{"", "-1", "01", "1.0", "18446744073709551616"} {
		p := good
		p.Generation = g
		if _, err := p.Validate(); err == nil {
			t.Fatalf("accepted generation %s", g)
		}
	}
	if (RuleChangeRequest{RulePlan: good, OperationID: "o"}).Validate() == nil {
		t.Fatal("accepted missing reviewed proofs")
	}
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "unused:unused@tcp(127.0.0.1:1)/unused", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var rows []models.RuleVersion
	q := versionListQuery(db, "rule", Reference{ID: "a", Version: "v"}).Find(&rows)
	sql := q.Statement.SQL.String()
	for _, fragment := range []string{"rule_id = ?", "rule_id > ? OR (rule_id = ? AND version > ?)", "ORDER BY rule_id ASC, version ASC", "LIMIT ?"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("query missing %s: %s", fragment, sql)
		}
	}
	if strings.Contains(sql, "content_json") || q.Statement.Vars[len(q.Statement.Vars)-1] != 21 {
		t.Fatalf("unbounded list: %s %v", sql, q.Statement.Vars)
	}
	if _, err := NewAdmin(db); err == nil {
		t.Fatal("accepted DryRun admin")
	}
}

func TestAdminMySQLReviewCASRecoveryAndPagination(t *testing.T) {
	r, db, ref, prefix, proof := reviewVerifiedRule(t)
	ctx := context.Background()
	actor := Actor{"operator", "oa"}
	admin, err := NewAdmin(db)
	if err != nil {
		t.Fatal(err)
	}
	bindings := map[string]Reference{"list": ref}
	plan := RulePlan{Kind: "publish", Site: prefix, Generation: "0", Bindings: bindings}
	preview, err := admin.Preview(ctx, plan)
	if err != nil || preview.VerificationIDs["list"] != proof.ID {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	updated, err := r.Verify(ctx, ref, prefix, actor)
	if err != nil {
		t.Fatal(err)
	}
	request := RuleChangeRequest{RulePlan: plan, OperationID: prefix + "-stale", VerificationIDs: preview.VerificationIDs}
	if _, err = admin.Change(ctx, request, actor); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("stale review published: %v", err)
	}
	if _, err = r.Current(ctx, prefix); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("stale review moved pointer: %v", err)
	}
	request.VerificationIDs = map[string]string{"list": updated.ID}
	request.OperationID = prefix + "-first"
	first, err := admin.Change(ctx, request, actor)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewAdmin(db.Session(&gorm.Session{NewDB: true}))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := reopened.Change(ctx, request, actor)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retry after reopen: %+v %v", retry, err)
	}
	found, err := reopened.Operation(ctx, request.OperationID)
	if err != nil || found.ID != first.ID {
		t.Fatalf("operation lookup: %+v %v", found, err)
	}
	if _, err = reopened.Change(ctx, request, Actor{"other", "oa"}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("operator spoof/reuse: %v", err)
	}
	// Concurrent distinct requests share the reviewed generation: one commit.
	request.Generation = "1"
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := request
			p.OperationID = fmt.Sprintf("%s-c%d", prefix, i)
			_, err := admin.Change(ctx, p, actor)
			if err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrGenerationConflict) {
				t.Errorf("concurrent publish: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("commits=%d", won.Load())
	}
	rollback := RulePlan{Kind: "rollback", Site: prefix, Generation: "2"}
	rp, err := admin.Preview(ctx, rollback)
	if err != nil || rp.Target.ID != first.ID {
		t.Fatalf("rollback preview: %+v %v", rp, err)
	}
	back, err := admin.Change(ctx, RuleChangeRequest{RulePlan: rollback, OperationID: prefix + "-rollback"}, actor)
	if err != nil || back.Generation != "3" || back.PreviousReleaseID != "" {
		t.Fatalf("rollback: %+v %v", back, err)
	}
	// Bounded lexicographic pagination, deliberately not semantic version sorting.
	for i := 0; i < 23; i++ {
		if _, err := r.CreateDraft(ctx, mysqlRule(prefix+"-pages", fmt.Sprintf("%02d", i)), actor); err != nil {
			t.Fatal(err)
		}
	}
	page, err := admin.Versions(ctx, prefix+"-pages", Reference{})
	if err != nil || len(page.Items) != 20 || page.Next == nil {
		t.Fatalf("page1: %+v %v", page, err)
	}
	page2, err := admin.Versions(ctx, prefix+"-pages", *page.Next)
	if err != nil || len(page2.Items) != 3 || page2.Next != nil || page2.Items[0].Version != "20" {
		t.Fatalf("page2: %+v %v", page2, err)
	}
	detail, err := admin.Inspect(ctx, ref)
	if err != nil || detail.Verification.ID != updated.ID {
		t.Fatalf("inspect: %+v %v", detail, err)
	}
	history, err := admin.Releases(ctx, prefix, 3)
	if err != nil || history.Current.Generation != "3" || len(history.Items) != 2 || history.Items[0].Generation != "2" {
		t.Fatalf("history: %+v %v", history, err)
	}
}

func TestAdminMySQLSnapshotAndUnknownCommit(t *testing.T) {
	r, db, ref, prefix, proof := reviewVerifiedRule(t)
	ctx := context.Background()
	actor := Actor{"operator", "oa"}
	bindings := map[string]Reference{"list": ref}
	proofs := map[string]string{"list": proof.ID}
	first, err := r.PublishReviewed(ctx, prefix+"-1", prefix, 0, bindings, proofs, actor)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := NewAdmin(db)
	callback := prefix + "-snapshot"
	var once atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_sites" && once.CompareAndSwap(false, true) {
			if _, err := r.PublishReviewed(ctx, prefix+"-2", prefix, 1, bindings, proofs, actor); err != nil {
				t.Errorf("interleaved publish: %v", err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	page, err := admin.Releases(ctx, prefix, 0)
	if err != nil || page.Current.ID != first.ID || len(page.Items) != 1 {
		t.Fatalf("mixed snapshot: %+v %v", page, err)
	}
	page, err = admin.Releases(ctx, prefix, 0)
	if err != nil || page.Current.Generation != "2" || len(page.Items) != 2 {
		t.Fatalf("next snapshot: %+v %v", page, err)
	}
	sqlDB, _ := db.DB()
	armed := &atomic.Bool{}
	armed.Store(true)
	ackDB, err := gorm.Open(mysql.New(mysql.Config{Conn: &lostAckPool{ConnPool: sqlDB, armed: armed}, SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ackAdmin, err := NewAdmin(ackDB)
	if err != nil {
		t.Fatal(err)
	}
	req := RuleChangeRequest{RulePlan: RulePlan{Kind: "publish", Site: prefix, Generation: "2", Bindings: bindings}, OperationID: prefix + "-ack", VerificationIDs: proofs}
	published, err := ackAdmin.Change(ctx, req, actor)
	if err != nil || published.Generation != "3" {
		t.Fatalf("lost ack reconciliation: %+v %v", published, err)
	}
	again, err := admin.Change(ctx, req, actor)
	if err != nil || again.ID != published.ID {
		t.Fatalf("unknown commit duplicated: %+v %v", again, err)
	}
}

func TestAdminMySQLFailedReverifyRemainsVisible(t *testing.T) {
	r, db, ref, prefix, proof := reviewVerifiedRule(t)
	ctx := context.Background()
	actor := Actor{"operator", "oa"}
	if _, err := r.PublishReviewed(ctx, prefix+"-first", prefix, 0, map[string]Reference{"list": ref}, map[string]string{"list": proof.ID}, actor); err != nil {
		t.Fatal(err)
	}
	set, err := r.SampleSet(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	set.Fixtures[0].Expect.RowCount = 2
	if err := r.CreateSampleSet(ctx, prefix+"-bad", set.Fixtures, actor); err != nil {
		t.Fatal(err)
	}
	failed, err := r.Verify(ctx, ref, prefix+"-bad", actor)
	if !errors.Is(err, crawlrule.ErrFixtureFailed) || failed.Passed {
		t.Fatalf("failed fixture: %+v %v", failed, err)
	}
	a, _ := NewAdmin(db)
	detail, err := a.Inspect(ctx, ref)
	if err != nil || detail.Verification != nil || !detail.Version.EverPublished {
		t.Fatalf("eligibility: %+v %v", detail, err)
	}
	found := false
	for _, v := range detail.History {
		if v.ID == failed.ID && !v.Passed {
			found = true
		}
	}
	if !found {
		t.Fatal("failed verification disappeared from inspection")
	}
	if _, err := a.Preview(ctx, RulePlan{Kind: "publish", Site: prefix, Generation: "1", Bindings: map[string]Reference{"list": ref}}); !errors.Is(err, ErrUnverified) {
		t.Fatalf("failed reverify authorized preview: %v", err)
	}
	if _, err := r.Resolve(ctx, ref.ID, ref.Version); err != nil {
		t.Fatalf("pinned published rule became unrunnable: %v", err)
	}
}
