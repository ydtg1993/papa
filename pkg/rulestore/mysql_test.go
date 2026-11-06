package rulestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This test never silently substitutes SQLite or an in-memory fake for MySQL.
// Explicit DSN opt-in is required, and only a dedicated rule_test database is
// accepted. The test adds unique rows, never deletes or drops databases/tables.
func mysqlRepository(t *testing.T) (*Repository, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("PAPA_RULE_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_RULE_MYSQL_TEST_DSN not set; real MySQL integration not run")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !strings.Contains(cfg.DBName, "rule_test") {
		t.Fatal("integration DSN must name a dedicated rule_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqldb, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.AutoMigrate(&models.RuleVersion{}, &models.RuleSample{}, &models.RuleSampleSet{}, &models.RuleVerification{}, &models.RuleRelease{}, &models.RuleSite{}); err != nil {
		t.Fatal(err)
	}
	blobs, err := NewFileBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	r, err := New(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return r, db, newID()
}

func mysqlRule(id, version string) crawlrule.Rule {
	return crawlrule.Rule{SchemaVersion: 1, ID: id, Version: version, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "b", Validation: crawlrule.Validation{Required: true, MinLength: 1}}}}
}

var errLostCommitAck = errors.New("injected lost COMMIT acknowledgement")

type lostAckPool struct {
	gorm.ConnPool
	armed *atomic.Bool
}

func (p *lostAckPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.ConnPool.(*sql.DB).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lostAckTx{Tx: tx, armed: p.armed}, nil
}

type lostAckTx struct {
	*sql.Tx
	armed *atomic.Bool
}

func (tx *lostAckTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.armed.Swap(false) {
		return errLostCommitAck
	}
	return nil
}

func TestMySQLRepositoryPublicationRecoveryCASAndReports(t *testing.T) {
	r, db, prefix := mysqlRepository(t)
	ctx := context.Background()
	actor := Actor{"repository-test", "isolated mysql integration"}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) { return b, nil }}
	sample, err := r.AddSample(ctx, SampleInput{URL: "https://example.test/page?token=secret", StatusCode: 200, Body: []byte("<article><b>kept</b></article>")}, policy, actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Body) != 0 || strings.Contains(sample.URL, "secret") {
		t.Fatalf("sample metadata leaked body/query: %+v", sample)
	}
	goodSet, badSet := prefix+"-good", prefix+"-bad"
	fixture := StoredFixture{Name: "one", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "kept"}}}}
	if err := r.CreateSampleSet(ctx, goodSet, []StoredFixture{fixture}, actor); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSampleSet(ctx, goodSet, []StoredFixture{fixture}, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("set overwritten: %v", err)
	}
	fixture.Expect.Fields[0].Value = "wrong"
	if err := r.CreateSampleSet(ctx, badSet, []StoredFixture{fixture}, actor); err != nil {
		t.Fatal(err)
	}
	refs := []Reference{{ID: prefix + "-catalog", Version: "1"}, {ID: prefix + "-catalog", Version: "2"}, {ID: prefix + "-catalog", Version: "3"}, {ID: prefix + "-detail", Version: "1"}, {ID: prefix + "-new", Version: "1"}}
	for _, ref := range refs {
		if _, err := r.CreateDraft(ctx, mysqlRule(ref.ID, ref.Version), actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.CreateDraft(ctx, mysqlRule(refs[0].ID, refs[0].Version), actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("draft overwritten: %v", err)
	}
	if _, err := r.Resolve(ctx, refs[0].ID, refs[0].Version); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("draft resolved: %v", err)
	}
	if _, err := r.Resolve(ctx, refs[0].ID, refs[0].Version); !errors.Is(err, crawlrule.ErrUnverifiedRule) {
		t.Fatalf("unpublished error classification: %v", err)
	}
	site := prefix + "-site"
	bindings := func(ref Reference) map[string]Reference {
		return map[string]Reference{"catalog": ref, "detail": refs[3]}
	}
	if _, err := r.Publish(ctx, prefix+"-rejected", site, 0, bindings(refs[0]), actor); !errors.Is(err, ErrUnverified) {
		t.Fatalf("unverified published: %v", err)
	}
	failed, err := r.Verify(ctx, refs[0], badSet, actor)
	if !errors.Is(err, crawlrule.ErrFixtureFailed) || failed.ID == "" || failed.Passed {
		t.Fatalf("failure not reported: %+v %v", failed, err)
	}
	readFailed, err := r.Verification(ctx, failed.ID)
	if err != nil || readFailed.Passed || len(readFailed.Report.Results[0].Failures) == 0 {
		t.Fatalf("failed report not durable: %+v %v", readFailed, err)
	}
	for _, ref := range refs {
		if verified, err := r.Verify(ctx, ref, goodSet, actor); err != nil || !verified.Passed {
			t.Fatalf("verify %v: %+v %v", ref, verified, err)
		}
	}
	first, err := r.Publish(ctx, prefix+"-first", site, 0, bindings(refs[0]), actor)
	if err != nil || first.Generation != 1 || len(first.Bindings) != 2 || first.CreatedAt.IsZero() {
		t.Fatalf("first release: %+v %v", first, err)
	}
	if first.VerificationIDs["catalog"] == "" || first.VerificationIDs["detail"] == "" {
		t.Fatal("publication omitted verification provenance")
	}
	again, err := r.Publish(ctx, prefix+"-first", site, 0, bindings(refs[0]), actor)
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent retry changed publication: %+v %v", again, err)
	}
	if _, err := r.Publish(ctx, prefix+"-first", site, 0, bindings(refs[1]), actor); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("operation reuse accepted: %v", err)
	}
	if _, err := r.Publish(ctx, prefix+"-stale", site, 0, bindings(refs[1]), actor); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for i, ref := range refs[1:3] {
		wg.Add(1)
		go func(i int, ref Reference) {
			defer wg.Done()
			_, err := r.Publish(ctx, prefix+"-race-"+ref.Version, site, 1, bindings(ref), actor)
			errorsOut <- err
		}(i, ref)
	}
	wg.Wait()
	close(errorsOut)
	wins, conflicts := 0, 0
	for err := range errorsOut {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrGenerationConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins, conflicts)
	}
	current, err := r.Current(ctx, site)
	if err != nil || current.Generation != 2 || current.Bindings["detail"] != refs[3] {
		t.Fatalf("torn release: %+v %v", current, err)
	}
	current.Bindings["catalog"] = Reference{ID: "mutated", Version: "mutated"}
	restarted, err := New(db, r.blobs)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restarted.Snapshot(ctx, site)
	if err != nil || snapshot.Generation != 2 || snapshot.Bindings["catalog"].ID == "mutated" {
		t.Fatalf("restart snapshot: %+v %v", snapshot, err)
	}
	if _, err := restarted.Resolve(ctx, refs[0].ID, refs[0].Version); err != nil {
		t.Fatalf("old pinned version lost after publication: %v", err)
	}
	if _, err := restarted.Verify(ctx, refs[0], badSet, actor); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatal(err)
	}
	if _, err := restarted.Resolve(ctx, refs[0].ID, refs[0].Version); err != nil {
		t.Fatalf("failed later verify revoked published version: %v", err)
	}
	rolled, err := restarted.Rollback(ctx, prefix+"-rollback", site, 2, actor)
	if err != nil || rolled.Bindings["catalog"] != refs[0] || rolled.Generation != 3 {
		t.Fatalf("rollback rejected formerly published version: %+v %v", rolled, err)
	}
	if rolled.VerificationIDs["catalog"] != first.VerificationIDs["catalog"] {
		t.Fatal("rollback replaced historical verification provenance")
	}
	if _, err := restarted.Rollback(ctx, prefix+"-rollback-end", site, 3, actor); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("rollback toggled: %v", err)
	}
	history, err := restarted.VerifyHistory(ctx, refs[0], 10)
	if err != nil || len(history) != 3 {
		t.Fatalf("verification history lost: %d %v", len(history), err)
	}
	if got, err := restarted.SampleSet(ctx, goodSet); err != nil || got.Fixtures[0].Expect.Fields[0].Value != "kept" {
		t.Fatalf("fixture mutated: %+v %v", got, err)
	}
	if _, err := restarted.Resolve(ctx, refs[0].ID, "missing"); !errors.Is(err, crawlrule.ErrVersionNotFound) {
		t.Fatalf("missing version classification: %v", err)
	}

	t.Run("three generations rollback consumes lineage", func(t *testing.T) {
		newProof, err := r.Verify(ctx, refs[0], goodSet, actor)
		if err != nil {
			t.Fatal(err)
		}
		original, err := r.LookupOperation(ctx, prefix+"-first")
		if err != nil || original.VerificationIDs["catalog"] != first.VerificationIDs["catalog"] || original.VerificationIDs["catalog"] == newProof.ID {
			t.Fatalf("later verification rewrote publication audit: %+v %v", original, err)
		}
		lineage := prefix + "-lineage"
		for i, ref := range refs[:3] {
			if _, err := r.Publish(ctx, prefix+"-lineage-"+ref.Version, lineage, uint64(i), bindings(ref), actor); err != nil {
				t.Fatal(err)
			}
		}
		for i, want := range []Reference{refs[1], refs[0]} {
			got, err := r.Rollback(ctx, prefix+"-back-"+want.Version, lineage, uint64(3+i), actor)
			if err != nil || got.Bindings["catalog"] != want {
				t.Fatalf("lineage: %+v %v", got, err)
			}
		}
		if _, err := r.Rollback(ctx, prefix+"-no-back", lineage, 5, actor); !errors.Is(err, ErrNoRollback) {
			t.Fatalf("went forward: %v", err)
		}
	})

	t.Run("wrong version proof and incompatible verifier reject publication", func(t *testing.T) {
		var candidate, other models.RuleVersion
		if err := versionScope(db, refs[4]).Take(&candidate).Error; err != nil {
			t.Fatal(err)
		}
		if err := versionScope(db, refs[1]).Take(&other).Error; err != nil {
			t.Fatal(err)
		}
		if err := versionScope(db.Model(&models.RuleVersion{}), refs[4]).Update("verification_id", other.VerificationID).Error; err != nil {
			t.Fatal(err)
		}
		_, err := r.Publish(ctx, prefix+"-mismatched-proof", prefix+"-proof-site", 0, bindings(refs[4]), actor)
		if restoreErr := versionScope(db.Model(&models.RuleVersion{}), refs[4]).Update("verification_id", candidate.VerificationID).Error; restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if !errors.Is(err, ErrUnverified) {
			t.Fatalf("another rule's proof accepted: %v", err)
		}
		if err := db.Model(&models.RuleVerification{}).Where("id = ?", candidate.VerificationID).Update("verifier_version", "future-unknown").Error; err != nil {
			t.Fatal(err)
		}
		_, err = r.Publish(ctx, prefix+"-wrong-verifier", prefix+"-proof-site", 0, bindings(refs[4]), actor)
		if restoreErr := db.Model(&models.RuleVerification{}).Where("id = ?", candidate.VerificationID).Update("verifier_version", VerifierVersion).Error; restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if !errors.Is(err, ErrUnverified) {
			t.Fatalf("unknown verifier proof accepted: %v", err)
		}
	})

	t.Run("failure after release insert rolls entire transaction back", func(t *testing.T) {
		injected := errors.New("injected site update failure")
		const callback = "rulestore:test-site-failure"
		if err := db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "crawler_rule_sites" {
				tx.AddError(injected)
			}
		}); err != nil {
			t.Fatal(err)
		}
		_, err := r.Publish(ctx, prefix+"-write-failure", site, 3, bindings(refs[4]), actor)
		_ = db.Callback().Update().Remove(callback)
		if !errors.Is(err, injected) {
			t.Fatalf("database failure reported success: %v", err)
		}
		if got, err := r.Current(ctx, site); err != nil || got.Generation != 3 {
			t.Fatalf("pointer changed on failed transaction: %+v %v", got, err)
		}
		if _, err := r.LookupOperation(ctx, prefix+"-write-failure"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed transaction persisted release: %v", err)
		}
		if _, err := r.Resolve(ctx, refs[4].ID, refs[4].Version); !errors.Is(err, ErrNotPublished) {
			t.Fatalf("failed transaction published rule: %v", err)
		}
	})

	t.Run("verification write failure is not success", func(t *testing.T) {
		injected := errors.New("injected report insert failure")
		const callback = "rulestore:test-report-failure"
		if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "crawler_rule_verifications" {
				tx.AddError(injected)
			}
		}); err != nil {
			t.Fatal(err)
		}
		got, err := r.Verify(ctx, refs[4], goodSet, actor)
		_ = db.Callback().Create().Remove(callback)
		if !errors.Is(err, injected) || got.Passed || got.ID != "" {
			t.Fatalf("unpersisted verification returned success: %+v %v", got, err)
		}
	})

	t.Run("lost commit acknowledgement recovered by durable operation", func(t *testing.T) {
		sqldb, _ := db.DB()
		armed := new(atomic.Bool)
		armed.Store(true)
		wrapped, err := gorm.Open(mysql.New(mysql.Config{Conn: &lostAckPool{ConnPool: sqldb, armed: armed}, SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := New(wrapped, r.blobs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := repo.Publish(ctx, prefix+"-lost-ack", site, 3, bindings(refs[1]), actor)
		if err != nil || got.Generation != 4 || armed.Load() {
			t.Fatalf("commit acknowledgement loss: %+v %v", got, err)
		}
		again, err := r.Publish(ctx, prefix+"-lost-ack", site, 3, bindings(refs[1]), actor)
		if err != nil || again.ID != got.ID {
			t.Fatalf("retry after lost ack: %+v %v", again, err)
		}
	})

	t.Run("same operation concurrent retries create one release", func(t *testing.T) {
		concurrentSite := prefix + "-same-op"
		results := make(chan Release, 2)
		errs := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				got, err := r.Publish(ctx, prefix+"-same-op", concurrentSite, 0, bindings(refs[1]), actor)
				results <- got
				errs <- err
			}()
		}
		wait.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		id := ""
		for result := range results {
			if id != "" && id != result.ID {
				t.Fatal("same operation created multiple releases")
			}
			id = result.ID
		}
		history, err := r.History(ctx, concurrentSite, 10)
		if err != nil || len(history) != 1 {
			t.Fatalf("duplicate history: %d %v", len(history), err)
		}
	})

	t.Run("repeatable read sees proof committed after earlier stage read", func(t *testing.T) {
		a := Reference{ID: prefix + "-rr-a", Version: "1"}
		b := Reference{ID: prefix + "-rr-b", Version: "1"}
		for _, ref := range []Reference{a, b} {
			if _, err := r.CreateDraft(ctx, mysqlRule(ref.ID, ref.Version), actor); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Verify(ctx, ref, goodSet, actor); err != nil {
				t.Fatal(err)
			}
		}
		// Under MySQL REPEATABLE READ an ordinary read of A's proof creates a
		// snapshot. Commit a fresh B verification at exactly that point, before
		// publication locks B and obtains its new VerificationID. Reading B's
		// proof from the old snapshot would falsely report record not found.
		const callback = "rulestore:test-repeatable-read"
		var once sync.Once
		var newProof Verification
		var verifyErr error
		if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
			proof, ok := tx.Statement.Dest.(*models.RuleVerification)
			if !ok || proof.RuleID != a.ID || tx.Error != nil {
				return
			}
			once.Do(func() { newProof, verifyErr = r.Verify(ctx, b, goodSet, actor) })
		}); err != nil {
			t.Fatal(err)
		}
		got, err := r.Publish(ctx, prefix+"-rr-publish", prefix+"-rr-site", 0, map[string]Reference{"a": a, "b": b}, actor)
		_ = db.Callback().Query().Remove(callback)
		if verifyErr != nil || newProof.ID == "" {
			t.Fatalf("concurrent verification was not committed: %+v %v", newProof, verifyErr)
		}
		if err != nil || got.VerificationIDs["b"] != newProof.ID {
			t.Fatalf("publication read stale verification snapshot: %+v %v", got, err)
		}
	})

	t.Run("empty explicit sample and failed body report", func(t *testing.T) {
		empty, err := r.AddSample(ctx, SampleInput{URL: "https://example.test/empty", StatusCode: 200, Body: []byte{}}, policy, actor)
		if err != nil || empty.BodyBytes != 0 {
			t.Fatalf("empty retained sample: %+v %v", empty, err)
		}
		emptyFixture := StoredFixture{Name: "empty", SampleID: empty.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "kept"}}}}
		if err := r.CreateSampleSet(ctx, prefix+"-empty", []StoredFixture{emptyFixture}, actor); err != nil {
			t.Fatal(err)
		}
		got, err := r.Verify(ctx, refs[4], prefix+"-empty", actor)
		if !errors.Is(err, crawlrule.ErrFixtureFailed) || got.ID == "" {
			t.Fatalf("empty sample failure not persisted: %+v %v", got, err)
		}
		if _, err := r.Verification(ctx, got.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("binary identifiers preserve case", func(t *testing.T) {
		for _, version := range []string{"V1", "v1"} {
			if _, err := r.CreateDraft(ctx, mysqlRule(prefix+"-case", version), actor); err != nil {
				t.Fatalf("case-sensitive version rejected: %v", err)
			}
		}
	})

	t.Run("JSON number fixture survives persistence without precision loss", func(t *testing.T) {
		num := json.Number("9007199254740993123")
		sample, err := r.AddSample(ctx, SampleInput{URL: "https://example.test/json", StatusCode: 200, Body: []byte(`[{"id":9007199254740993123}]`)}, policy, actor)
		if err != nil {
			t.Fatal(err)
		}
		ref := Reference{ID: prefix + "-number", Version: "1"}
		if _, err := r.CreateDraft(ctx, crawlrule.Rule{SchemaVersion: 1, ID: ref.ID, Version: ref.Version, Format: "json", Rows: "", Fields: []crawlrule.Field{{Name: "id", Pointer: "/id"}}}, actor); err != nil {
			t.Fatal(err)
		}
		if err := r.CreateSampleSet(ctx, prefix+"-number", []StoredFixture{{Name: "precise", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/id", Value: num}}}}}, actor); err != nil {
			t.Fatal(err)
		}
		if got, err := r.Verify(ctx, ref, prefix+"-number", actor); err != nil || !got.Passed {
			t.Fatalf("number lost precision: %+v %v", got, err)
		}
		set, err := r.SampleSet(ctx, prefix+"-number")
		if err != nil || set.Fixtures[0].Expect.Fields[0].Value != num {
			t.Fatalf("stored number changed: %+v %v", set, err)
		}
	})

	t.Run("Resolve and Snapshot remain read only", func(t *testing.T) {
		injected := errors.New("write attempted in read-only resolution")
		const callback = "rulestore:test-read-only"
		reject := func(tx *gorm.DB) { tx.AddError(injected) }
		if err := db.Callback().Create().Before("gorm:create").Register(callback, reject); err != nil {
			t.Fatal(err)
		}
		if err := db.Callback().Update().Before("gorm:update").Register(callback, reject); err != nil {
			t.Fatal(err)
		}
		if err := db.Callback().Delete().Before("gorm:delete").Register(callback, reject); err != nil {
			t.Fatal(err)
		}
		defer db.Callback().Create().Remove(callback)
		defer db.Callback().Update().Remove(callback)
		defer db.Callback().Delete().Remove(callback)
		if _, err := r.Resolve(ctx, refs[0].ID, refs[0].Version); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Snapshot(ctx, site); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("imported snapshot outlives archive body and keeps provenance", func(t *testing.T) {
		archiveDir := t.TempDir()
		evidenceID := strings.Repeat("a", 32)
		body := []byte("<article><b>kept</b></article>")
		bodyDigest := digest(body)
		for _, sub := range []string{"observations", "bodies"} {
			if err := os.MkdirAll(filepath.Join(archiveDir, ".rule-snapshots", sub), 0700); err != nil {
				t.Fatal(err)
			}
		}
		bodyRelative := ".rule-snapshots/bodies/" + bodyDigest + ".body"
		bodyPath := filepath.Join(archiveDir, filepath.FromSlash(bodyRelative))
		manifestPath := filepath.Join(archiveDir, ".rule-snapshots", "observations", evidenceID+".json")
		if err := os.WriteFile(bodyPath, body, 0600); err != nil {
			t.Fatal(err)
		}
		manifest, _ := json.Marshal(map[string]any{"version": 1, "evidence_id": evidenceID, "source_url": "https://example.test/rendered", "status_code": 0, "rendered": true, "body_file": bodyRelative, "body_sha256": bodyDigest, "body_bytes": len(body)})
		if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
			t.Fatal(err)
		}
		imported, err := r.ImportSnapshot(ctx, archiveDir, evidenceID, policy, actor)
		if err != nil || imported.EvidenceID != evidenceID || !imported.Rendered || imported.StatusCode != 0 {
			t.Fatalf("snapshot provenance lost: %+v %v", imported, err)
		}
		fixture := StoredFixture{Name: "rendered", SampleID: imported.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "kept"}}}}
		if err := r.CreateSampleSet(ctx, prefix+"-snapshot", []StoredFixture{fixture}, actor); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(bodyPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(manifestPath); err != nil {
			t.Fatal(err)
		}
		if got, err := r.blobs.Get(ctx, imported.BodySHA256, MaxSampleBodyBytes); err != nil || string(got) != string(body) {
			t.Fatalf("archive deletion removed durable body: %q %v", got, err)
		}
		if got, err := r.Verify(ctx, refs[4], prefix+"-snapshot", actor); err != nil || !got.Passed {
			t.Fatalf("imported DOM verification after archive GC: %+v %v", got, err)
		}
	})

	t.Run("rule and set corruption reject loading and publishing", func(t *testing.T) {
		if err := versionScope(db.Model(&models.RuleVersion{}), refs[4]).Update("content_json", "{}").Error; err != nil {
			t.Fatal(err)
		}
		if _, err := r.Get(ctx, refs[4]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("tampered rule accepted: %v", err)
		}
		if err := db.Model(&models.RuleSampleSet{}).Where("set_id = ?", goodSet).Update("fixtures_json", "{}").Error; err != nil {
			t.Fatal(err)
		}
		if _, err := r.Publish(ctx, prefix+"-corrupt", site, 4, bindings(refs[1]), actor); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("tampered proof set accepted: %v", err)
		}
	})
}

func TestRepositoryMySQLSQLShapes(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "root@tcp(127.0.0.1:9)/none", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	cas := advanceSite(db, "shop", 2, "release").Statement
	sql := cas.SQL.String()
	if !strings.Contains(sql, "`generation`=`generation` + 1") || !strings.Contains(sql, "site = ? AND generation = ?") {
		t.Fatalf("CAS SQL shape: %s", sql)
	}
	insert := ensureSite(db, "shop").Statement.SQL.String()
	if !strings.Contains(insert, "ON DUPLICATE KEY UPDATE `site`=`site`") {
		t.Fatalf("first publication lock shape: %s", insert)
	}
	if _, err := New(db, &FileBlobs{}); err == nil {
		t.Fatal("dry run DB accepted as durable store")
	}
}

func TestRepositoryValidationAndIntegrityWithoutDatabase(t *testing.T) {
	for _, actor := range []Actor{{}, {"operator", ""}, {"operator", strings.Repeat("x", 257)}, {" operator", "source"}} {
		if validateActor(actor) == nil {
			t.Fatalf("invalid actor accepted: %+v", actor)
		}
	}
	if validateBindings(map[string]Reference{"catalog": {ID: " ", Version: "1"}}) == nil {
		t.Fatal("blank reference accepted")
	}
	data, _ := encode(bindingsEncoding{SchemaVersion: 1, Bindings: map[string]Reference{"catalog": {ID: "rule", Version: "1"}}, VerificationIDs: map[string]string{"catalog": "proof"}}, MaxMetadataBytes)
	row := models.RuleRelease{ID: "release", Site: "shop", Generation: 1, Kind: "publish", BindingsJSON: string(data), BindingsDigest: digest(data)}
	if _, err := releaseFromRow(row); err != nil {
		t.Fatal(err)
	}
	row.BindingsJSON = "{}"
	if _, err := releaseFromRow(row); !errors.Is(err, ErrCorrupt) {
		t.Fatal("release corruption accepted")
	}
	for _, report := range []crawlrule.FixtureReport{
		{Passed: true, Results: []crawlrule.FixtureResult{{Name: "one", Passed: false, Failures: []string{"mismatch"}}}},
		{Passed: true, Results: []crawlrule.FixtureResult{{Name: "one", Passed: true, Failures: []string{"mismatch"}}}},
	} {
		data, _ := encode(report, MaxMetadataBytes)
		if _, err := publicVerification(models.RuleVerification{Passed: true, ReportJSON: string(data)}); !errors.Is(err, ErrCorrupt) {
			t.Fatal("internally inconsistent successful proof accepted")
		}
	}
}
