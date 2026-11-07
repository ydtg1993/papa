package rulestore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"gorm.io/gorm"
)

func TestCreateSampleSetDoesNotMutateCallerExpectations(t *testing.T) {
	value := map[string]any{"id": int64(42)}
	fixtures := []StoredFixture{{
		// Validation deliberately stops before persistence, after detachment.
		Name: "", SampleID: "sample",
		Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/value", Value: value}}},
	}}
	want := map[string]any{"id": int64(42)}
	err := new(Repository).CreateSampleSet(context.Background(), "sample-set", fixtures, Actor{"review", "regression"})
	if err == nil {
		t.Fatal("invalid fixture accepted")
	}
	if !reflect.DeepEqual(fixtures[0].Expect.Fields[0].Value, want) || !reflect.DeepEqual(value, want) {
		t.Fatalf("CreateSampleSet mutated caller expectation: %#v; original map: %#v", fixtures[0].Expect.Fields[0].Value, value)
	}
}

func TestMySQLSampleMetadataPreservesAcceptedURL(t *testing.T) {
	r, _, _ := mysqlRepository(t)
	input := SampleInput{URL: "https://example.test/" + strings.Repeat("&", 12000), StatusCode: 200, Body: []byte("<b>sample</b>")}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) { return body, nil }}
	if _, err := SanitizeSample(input, policy); err != nil {
		t.Fatalf("test input should be within the sample budget: %v", err)
	}
	sample, err := r.AddSample(context.Background(), input, policy, Actor{"review", "long accepted URL"})
	if err != nil {
		t.Fatalf("accepted sample metadata could not be persisted: %v", err)
	}
	if sample.URL != input.URL {
		t.Fatal("sample URL changed after persistence")
	}
}

func TestMySQLSampleInsertFailureRetainsSharedBlobAndCancellation(t *testing.T) {
	r, db, prefix := mysqlRepository(t)
	ctx := context.Background()
	actor := Actor{"review", "failure and cancellation"}
	input := SampleInput{URL: "https://example.test/sample", StatusCode: 200, Body: []byte("<b>shared-body-" + prefix + "</b>")}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) { return body, nil }}
	injected := errors.New("injected sample metadata insert failure")
	const callback = "rulestore:review-sample-insert-failure"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == (models.RuleSample{}).TableName() {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	got, err := r.AddSample(ctx, input, policy, actor)
	if !errors.Is(err, injected) || got.ID != "" {
		t.Fatalf("failed metadata insert returned a sample: %+v %v", got, err)
	}
	if body, err := r.blobs.Get(ctx, digest(input.Body), MaxSampleBodyBytes); err != nil || string(body) != string(input.Body) {
		t.Fatalf("failed insert lost the published reusable blob: %q %v", body, err)
	}
	_ = db.Callback().Create().Remove(callback)
	if _, err := r.AddSample(ctx, input, policy, actor); err != nil {
		t.Fatalf("retry could not reuse the retained blob: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := r.AddSample(cancelled, input, policy, actor); !errors.Is(err, context.Canceled) || got.ID != "" {
		t.Fatalf("cancelled insert returned a sample: %+v %v", got, err)
	}
	var count int64
	if err := db.Model(&models.RuleSample{}).Where("digest = ?", digest(input.Body)).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("failure or cancellation inserted metadata: count=%d err=%v", count, err)
	}
}

func reviewVerifiedRule(t *testing.T) (*Repository, *gorm.DB, Reference, string, Verification) {
	t.Helper()
	r, db, prefix := mysqlRepository(t)
	ctx := context.Background()
	actor := Actor{"review", "verification integrity"}
	ref := Reference{ID: prefix, Version: "1"}
	if _, err := r.CreateDraft(ctx, mysqlRule(ref.ID, ref.Version), actor); err != nil {
		t.Fatal(err)
	}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) { return body, nil }}
	sample, err := r.AddSample(ctx, SampleInput{URL: "https://example.test/sample", StatusCode: 200, Body: []byte("<article><b>kept</b></article>")}, policy, actor)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []StoredFixture{
		{Name: "first", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "kept"}}}},
		{Name: "second", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "kept"}}}},
	}
	if err := r.CreateSampleSet(ctx, prefix, fixtures, actor); err != nil {
		t.Fatal(err)
	}
	proof, err := r.Verify(ctx, ref, prefix, actor)
	if err != nil {
		t.Fatal(err)
	}
	return r, db, ref, prefix, proof
}

func TestMySQLPublishRejectsProofReportNotMatchingSet(t *testing.T) {
	r, db, ref, prefix, proof := reviewVerifiedRule(t)
	for _, tt := range []struct {
		name string
		edit func(*crawlrule.FixtureReport)
	}{
		{"missing fixture", func(report *crawlrule.FixtureReport) { report.Results = report.Results[:1] }},
		{"renamed fixture", func(report *crawlrule.FixtureReport) { report.Results[0].Name = "other" }},
		{"wrong row count", func(report *crawlrule.FixtureReport) { report.Results[0].Rows++ }},
		{"wrong truncation", func(report *crawlrule.FixtureReport) { report.Results[0].Truncated = true }},
		{"invalid rows accepted", func(report *crawlrule.FixtureReport) { report.Results[0].InvalidRows = 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := proof.Report
			report.Results = append([]crawlrule.FixtureResult(nil), report.Results...)
			tt.edit(&report)
			data, err := encode(report, MaxMetadataBytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.RuleVerification{}).Where("id = ?", proof.ID).Update("report_json", string(data)).Error; err != nil {
				t.Fatal(err)
			}
			got, err := r.Publish(context.Background(), prefix+tt.name, prefix+tt.name, 0, map[string]Reference{"catalog": ref}, Actor{"review", "corrupt proof"})
			if !errors.Is(err, ErrCorrupt) || got.ID != "" {
				t.Fatalf("proof report not matching frozen set authorized publication: release=%s err=%v", got.ID, err)
			}
		})
	}
}

func TestMySQLResolveClassifiesCorruptPublishedVersion(t *testing.T) {
	r, db, ref, prefix, _ := reviewVerifiedRule(t)
	ctx := context.Background()
	if _, err := r.Publish(ctx, prefix, prefix, 0, map[string]Reference{"catalog": ref}, Actor{"review", "corrupt version classification"}); err != nil {
		t.Fatal(err)
	}
	if err := versionScope(db.Model(&models.RuleVersion{}), ref).Update("content_json", "{}").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, ref.ID, ref.Version); !errors.Is(err, ErrCorrupt) || !errors.Is(err, crawlrule.ErrInvalidVersion) {
		t.Fatalf("corrupt stored version is not classified as permanent: %v", err)
	}
}

func TestMySQLVerifyCompletionOrderPreservesPublishedProof(t *testing.T) {
	r, db, ref, setID, original := reviewVerifiedRule(t)
	ctx := context.Background()
	actor := Actor{"review", "completion order"}
	bad, err := r.SampleSet(ctx, setID)
	if err != nil {
		t.Fatal(err)
	}
	bad.Fixtures[0].Expect.Fields[0].Value = "mismatch"
	badSetID := setID + "-bad"
	if err := r.CreateSampleSet(ctx, badSetID, bad.Fixtures, actor); err != nil {
		t.Fatal(err)
	}
	type pausedVerification struct{}
	olderContext := context.WithValue(ctx, pausedVerification{}, true)
	var published Release
	var failed Verification
	var publishErr, verifyErr error
	const callback = "rulestore:review-verification-order"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*models.RuleSampleSet); !ok || tx.Statement.Context.Value(pausedVerification{}) != true {
			return
		}
		// The older Verify has captured its inputs but has not completed. A
		// publication and a later failing Verify commit before it resumes.
		published, publishErr = r.Publish(ctx, setID, setID, 0, map[string]Reference{"catalog": ref}, actor)
		failed, verifyErr = r.Verify(ctx, ref, badSetID, actor)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	completedLast, err := r.Verify(olderContext, ref, setID, actor)
	_ = db.Callback().Query().Remove(callback)
	if publishErr != nil || published.VerificationIDs["catalog"] != original.ID {
		t.Fatalf("publication used a proof that had not completed: %+v %v", published, publishErr)
	}
	if !errors.Is(verifyErr, crawlrule.ErrFixtureFailed) || failed.ID == "" {
		t.Fatalf("overlapping failure was not durably recorded: %+v %v", failed, verifyErr)
	}
	if err != nil || !completedLast.Passed {
		t.Fatalf("last completed verification: %+v %v", completedLast, err)
	}
	var version models.RuleVersion
	if err := versionScope(db, ref).Take(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version.VerificationID != completedLast.ID || version.State != "published" || !version.EverPublished {
		t.Fatalf("completion order or publication marker lost: %+v", version)
	}
	history, err := r.VerifyHistory(ctx, ref, 10)
	if err != nil || len(history) != 3 {
		t.Fatalf("overlapping verification audit lost: count=%d err=%v", len(history), err)
	}
	current, err := r.Current(ctx, setID)
	if err != nil || current.VerificationIDs["catalog"] != original.ID {
		t.Fatalf("late verification rewrote published proof: %+v %v", current, err)
	}
}

func TestMySQLVerifyCancellationDoesNotReplaceProof(t *testing.T) {
	r, db, ref, setID, original := reviewVerifiedRule(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callback = "rulestore:review-verification-cancellation"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*models.RuleSample); ok && tx.Statement.Context == ctx {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	got, err := r.Verify(ctx, ref, setID, Actor{"review", "cancelled verification"})
	_ = db.Callback().Query().Remove(callback)
	if !errors.Is(err, context.Canceled) || got.ID != "" {
		t.Fatalf("cancelled verification returned a durable result: %+v %v", got, err)
	}
	var version models.RuleVersion
	if err := versionScope(db, ref).Take(&version).Error; err != nil {
		t.Fatal(err)
	}
	history, err := r.VerifyHistory(context.Background(), ref, 10)
	if err != nil || len(history) != 1 || version.VerificationID != original.ID || version.State != "verified" {
		t.Fatalf("cancellation replaced proof: history=%d proof=%s state=%s err=%v", len(history), version.VerificationID, version.State, err)
	}
}
