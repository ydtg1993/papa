package rulestore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func TestMySQLNegativeFixturesLegacyReleaseAndRecovery(t *testing.T) {
	r, db, prefix := mysqlRepository(t)
	ctx := context.Background()
	actor := Actor{"d2-test", "synthetic negative fixtures"}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) { return b, nil }}
	add := func(name, body string, status int, expect crawlrule.Expectations) StoredFixture {
		t.Helper()
		s, err := r.AddSample(ctx, SampleInput{URL: "https://fixture.example/" + name, StatusCode: status, Body: []byte(body)}, policy, actor)
		if err != nil {
			t.Fatal(err)
		}
		return StoredFixture{Name: name, SampleID: s.ID, Expect: expect}
	}
	pos := add("positive", "<article><b>kept</b></article>", 200, crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "kept"}}})
	empty := add("empty", "<main>empty</main>", 200, crawlrule.Expectations{Kind: "empty"})
	reject := add("denied", "denied", 403, crawlrule.Expectations{Kind: "reject", ErrorKind: "http_status", StatusCode: 403})
	invalid := add("validation", "<article><b></b></article>", 200, crawlrule.Expectations{Kind: "reject", ErrorKind: "validation"})
	if err := r.CreateSampleSet(ctx, prefix+"-negative-only", []StoredFixture{empty, reject}, actor); err == nil {
		t.Fatal("negative-only set accepted")
	}
	for _, set := range []struct {
		id       string
		fixtures []StoredFixture
	}{{prefix + "-old", []StoredFixture{pos}}, {prefix + "-mixed", []StoredFixture{pos, empty, reject, invalid}}} {
		if err := r.CreateSampleSet(ctx, set.id, set.fixtures, actor); err != nil {
			t.Fatal(err)
		}
	}
	oldRef := Reference{ID: prefix, Version: "old"}
	newRef := Reference{ID: prefix, Version: "new"}
	for _, ref := range []Reference{oldRef, newRef} {
		if _, err := r.CreateDraft(ctx, mysqlRule(ref.ID, ref.Version), actor); err != nil {
			t.Fatal(err)
		}
	}
	old, err := r.Verify(ctx, oldRef, prefix+"-old", actor)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce actual v2 report bytes, retaining the original positive set.
	legacyReport := old.Report
	legacyReport.Positive, legacyReport.Negative, legacyReport.Failures = 0, 0, nil
	raw, _ := json.Marshal(legacyReport)
	if err := db.Model(&models.RuleVerification{}).Where("id = ?", old.ID).Updates(map[string]any{"verifier_version": "crawlrule-fixtures-v2", "report_json": string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	first, err := r.PublishReviewed(ctx, prefix+"-publish-old", prefix, 0, map[string]Reference{"catalog": oldRef}, map[string]string{"catalog": old.ID}, actor)
	if err != nil {
		t.Fatalf("legacy publish: %v", err)
	}
	proof, err := r.Verify(ctx, newRef, prefix+"-mixed", actor)
	if err != nil || !proof.Passed || proof.Report.Positive != 1 || proof.Report.Negative != 3 {
		t.Fatalf("mixed proof: %+v %v", proof, err)
	}
	// Verify is serialized with the rule lock; every successful immutable proof
	// remains readable while only the latest identity is publishable.
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Verify(ctx, newRef, prefix+"-mixed", actor); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.PublishReviewed(ctx, prefix+"-stale", prefix, first.Generation, map[string]Reference{"catalog": newRef}, map[string]string{"catalog": proof.ID}, actor); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("stale review accepted: %v", err)
	}
	var latest models.RuleVersion
	if err := versionScope(db, newRef).Take(&latest).Error; err != nil {
		t.Fatal(err)
	}
	proof, err = r.Verification(ctx, latest.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.PublishReviewed(ctx, prefix+"-publish-new", prefix, first.Generation, map[string]Reference{"catalog": newRef}, map[string]string{"catalog": proof.ID}, actor)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(db, r.blobs)
	if err != nil {
		t.Fatal(err)
	}
	current, err := reopened.Current(ctx, prefix)
	if err != nil || current.ID != second.ID {
		t.Fatalf("current reopen: %+v %v", current, err)
	}
	if v, err := reopened.Verification(ctx, proof.ID); err != nil || v.Report.Negative != 3 {
		t.Fatalf("report reopen: %+v %v", v, err)
	}
	rolled, err := reopened.Rollback(ctx, prefix+"-rollback", prefix, second.Generation, actor)
	if err != nil || rolled.Bindings["catalog"] != oldRef {
		t.Fatalf("legacy rollback: %+v %v", rolled, err)
	}
	// A self-consistent result with a different rejection code must still fail
	// the immutable set binding when publishing again.
	corrupt := proof.Report
	corrupt.Results = append([]crawlrule.FixtureResult(nil), proof.Report.Results...)
	corrupt.Results[2].StatusCode = 401
	data, _ := json.Marshal(corrupt)
	if err := db.Model(&models.RuleVerification{}).Where("id = ?", proof.ID).Update("report_json", string(data)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishReviewed(ctx, prefix+"-corrupt", prefix, rolled.Generation, map[string]Reference{"catalog": newRef}, map[string]string{"catalog": proof.ID}, actor); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mismatched negative proof published: %v", err)
	}
	original, _ := json.Marshal(proof.Report)
	if err := db.Model(&models.RuleVerification{}).Where("id = ?", proof.ID).Update("report_json", string(original)).Error; err != nil {
		t.Fatal(err)
	}
	// Corrupt only a synthetic sample in this test's private temporary blob root.
	sample, err := r.Sample(ctx, empty.SampleID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.blobs.root.Remove(sample.BodySHA256); err != nil {
		t.Fatal(err)
	}
	failed, err := r.Verify(ctx, newRef, prefix+"-mixed", actor)
	if !errors.Is(err, crawlrule.ErrFixtureFailed) || failed.Passed || failed.Report.Results[1].ErrorKind != "execution" {
		t.Fatalf("missing body accepted as negative: %+v %v", failed, err)
	}
}
