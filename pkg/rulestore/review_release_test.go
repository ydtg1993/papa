package rulestore

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func releaseReviewFixture(t *testing.T, r *Repository, prefix string) (Actor, []Reference) {
	t.Helper()
	ctx := context.Background()
	actor := Actor{"release-review", "isolated mysql regression"}
	sample, err := r.AddSample(ctx, SampleInput{URL: "https://example.test/page", StatusCode: 200, Body: []byte("<article><b>kept</b></article>")}, htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) { return body, nil }}, actor)
	if err != nil {
		t.Fatal(err)
	}
	setID := prefix + "-set"
	if err := r.CreateSampleSet(ctx, setID, []StoredFixture{{Name: "one", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "kept"}}}}}, actor); err != nil {
		t.Fatal(err)
	}
	refs := []Reference{{ID: prefix + "-rule", Version: "1"}, {ID: prefix + "-rule", Version: "2"}}
	for _, ref := range refs {
		if _, err := r.CreateDraft(ctx, mysqlRule(ref.ID, ref.Version), actor); err != nil {
			t.Fatal(err)
		}
		if result, err := r.Verify(ctx, ref, setID, actor); err != nil || !result.Passed {
			t.Fatalf("verify fixture: %+v %v", result, err)
		}
	}
	return actor, refs
}

type rejectedCommitPool struct {
	*sql.DB
	err error
}

func (p *rejectedCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &rejectedCommitTx{Tx: tx, err: p.err}, nil
}

type rejectedCommitTx struct {
	*sql.Tx
	err error
}

func (tx *rejectedCommitTx) Commit() error {
	if err := tx.Tx.Rollback(); err != nil {
		return err
	}
	return tx.err
}

func TestMySQLReleaseCommitErrorRequiresDurableOperation(t *testing.T) {
	r, db, prefix := mysqlRepository(t)
	actor, refs := releaseReviewFixture(t, r, prefix)
	sqldb, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("injected COMMIT rejected without persistence")
	wrapped, err := gorm.Open(mysql.New(mysql.Config{Conn: &rejectedCommitPool{DB: sqldb, err: rejected}, SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := New(wrapped, r.blobs)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opID, site := prefix+"-op", prefix+"-site"
	if got, err := repo.Publish(ctx, opID, site, 0, map[string]Reference{"catalog": refs[0]}, actor); !errors.Is(err, rejected) || got.ID != "" {
		t.Fatalf("uncommitted transaction reported a release: %+v %v", got, err)
	}
	if _, err := r.LookupOperation(ctx, opID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected COMMIT persisted operation: %v", err)
	}
	if _, err := r.Current(ctx, site); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("rejected COMMIT advanced current release: %v", err)
	}
	var version models.RuleVersion
	if err := versionScope(db, refs[0]).Take(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version.EverPublished || version.State != "verified" {
		t.Fatalf("rejected COMMIT changed version eligibility: %+v", version)
	}
	if got, err := r.Publish(ctx, opID, site, 0, map[string]Reference{"catalog": refs[0]}, actor); err != nil || got.Generation != 1 {
		t.Fatalf("same operation cannot safely retry rejected COMMIT: %+v %v", got, err)
	}
}

func TestMySQLReleaseOperationIDIsGlobalAcrossSites(t *testing.T) {
	r, _, prefix := mysqlRepository(t)
	actor, refs := releaseReviewFixture(t, r, prefix)
	ctx := context.Background()
	opID := prefix + "-op"
	// Both concurrent callers may safely share the same immutable input map.
	bindings := map[string]Reference{"catalog": refs[0]}
	type result struct {
		site    string
		ref     Reference
		release Release
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, suffix := range []string{"-a", "-b"} {
		workers.Add(1)
		go func(site string, ref Reference) {
			defer workers.Done()
			<-start
			got, err := r.Publish(ctx, opID, site, 0, bindings, actor)
			results <- result{site: site, ref: ref, release: got, err: err}
		}(prefix+suffix, refs[0])
	}
	close(start)
	workers.Wait()
	close(results)
	wins := 0
	for result := range results {
		if result.err == nil {
			wins++
			if retry, err := r.Publish(ctx, opID, result.site, 0, map[string]Reference{"catalog": result.ref}, actor); err != nil || retry.ID != result.release.ID {
				t.Fatalf("winning operation retry: %+v %v", retry, err)
			}
			continue
		}
		var mysqlErr *mysqldriver.MySQLError
		// Concurrent missing-key locking reads may deadlock under REPEATABLE
		// READ. The documented recovery is retrying the unchanged operation ID.
		if !errors.Is(result.err, ErrOperationConflict) && !(errors.As(result.err, &mysqlErr) && mysqlErr.Number == 1213) {
			t.Fatalf("unexpected overlapping operation failure: %v", result.err)
		}
		if _, err := r.Publish(ctx, opID, result.site, 0, map[string]Reference{"catalog": result.ref}, actor); !errors.Is(err, ErrOperationConflict) {
			t.Fatalf("cross-site operation reuse accepted after resolution: %v", err)
		}
		if _, err := r.Current(ctx, result.site); !errors.Is(err, ErrNoRelease) {
			t.Fatalf("losing site retained a publication: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("global operation has %d successful publications", wins)
	}
}

func TestMySQLReleaseRepeatedBindingsRollbackConsumesEveryPredecessor(t *testing.T) {
	r, _, prefix := mysqlRepository(t)
	actor, refs := releaseReviewFixture(t, r, prefix)
	ctx := context.Background()
	site := prefix + "-site"
	var published []Release
	for i, ref := range []Reference{refs[0], refs[0], refs[1]} {
		got, err := r.Publish(ctx, newID(), site, uint64(i), map[string]Reference{"catalog": ref}, actor)
		if err != nil {
			t.Fatal(err)
		}
		published = append(published, got)
	}
	for i := range 2 {
		got, err := r.Rollback(ctx, newID(), site, uint64(3+i), actor)
		if err != nil || got.Generation != uint64(4+i) || got.Bindings["catalog"] != refs[0] || got.VerificationIDs["catalog"] != published[1-i].VerificationIDs["catalog"] || got.PreviousReleaseID != published[1-i].PreviousReleaseID {
			t.Fatalf("repeated bindings lost predecessor %d: %+v %v", i, got, err)
		}
	}
	if _, err := r.Rollback(ctx, newID(), site, 5, actor); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("exhausted lineage rolled forward: %v", err)
	}
	if history, err := r.History(ctx, site, 10); err != nil || len(history) != 5 {
		t.Fatalf("rollback changed append-only history: %+v %v", history, err)
	}
}

func TestMySQLReleaseDetachesBindingsBeforeTransaction(t *testing.T) {
	r, db, prefix := mysqlRepository(t)
	actor, refs := releaseReviewFixture(t, r, prefix)
	bindings := map[string]Reference{"catalog": refs[0]}
	const callback = "rulestore:review-mutate-caller-bindings"
	mutated := false
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_sites" {
			// The API has already encoded the request before opening its transaction.
			// Mutate on the same goroutine so this deterministically checks ownership,
			// without introducing an unsynchronized test-side map race.
			bindings["catalog"] = refs[1]
			mutated = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	got, err := r.Publish(context.Background(), prefix+"-op", prefix+"-site", 0, bindings, actor)
	if err != nil {
		t.Fatal(err)
	}
	if !mutated || got.Bindings["catalog"] != refs[0] {
		t.Fatalf("publication no longer matches the encoded request: mutated=%v got=%+v want=%+v", mutated, got.Bindings["catalog"], refs[0])
	}
	retry, err := r.Publish(context.Background(), prefix+"-op", prefix+"-site", 0, map[string]Reference{"catalog": refs[0]}, actor)
	if err != nil || retry.ID != got.ID || retry.Bindings["catalog"] != refs[0] {
		t.Fatalf("idempotent result differs from request: %+v %v", retry, err)
	}
}
