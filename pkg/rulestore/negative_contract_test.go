package rulestore

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestNegativeFixtureEncodingAndProofIdentity(t *testing.T) {
	old := `{"RowCount":1,"Fields":[{"Row":0,"Pointer":"/title","Value":"A"}],"NextURL":"","NextCursor":"","Truncated":false}`
	var expectation crawlrule.Expectations
	if err := json.Unmarshal([]byte(old), &expectation); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(expectation)
	if string(raw) != old {
		t.Fatalf("legacy expectation bytes changed: %s", raw)
	}
	set := setEncoding{SchemaVersion: 2, Fixtures: []fixtureEncoding{
		{StoredFixture: StoredFixture{Name: "positive", Expect: expectation}},
		{StoredFixture: StoredFixture{Name: "denied", Expect: crawlrule.Expectations{Kind: "reject", ErrorKind: "http_status", StatusCode: 403}}},
	}}
	report := crawlrule.SummarizeFixtures([]crawlrule.FixtureResult{{Name: "positive", Passed: true, Rows: 1}, {Name: "denied", Kind: "reject", ErrorKind: "http_status", StatusCode: 403, Passed: true}})
	bytes, _ := json.Marshal(report)
	row := models.RuleVerification{VerifierVersion: VerifierVersion, Passed: true, ReportJSON: string(bytes)}
	if _, err := publicVerification(row); err != nil {
		t.Fatal(err)
	}
	if err := validateVerificationSet(report, set); err != nil {
		t.Fatal(err)
	}
	report.Results[1].StatusCode = 401
	if !errors.Is(validateVerificationSet(report, set), ErrCorrupt) {
		t.Fatal("wrong rejection status authorized publication")
	}
	report.Results[1].StatusCode = 403
	report.Results[1].Kind = "empty"
	if !errors.Is(validateVerificationSet(report, set), ErrCorrupt) {
		t.Fatal("fixture kind swap authorized publication")
	}
	row.VerifierVersion = "crawlrule-fixtures-v2"
	if _, err := publicVerification(row); !errors.Is(err, ErrCorrupt) {
		t.Fatal("negative report disguised as legacy verifier")
	}
	legacy := models.RuleVerification{VerifierVersion: "crawlrule-fixtures-v2", Passed: true, ReportJSON: `{"Passed":true,"Results":[{"Name":"positive","Passed":true,"Rows":1,"InvalidRows":0,"Truncated":false,"Failures":null}]}`}
	if _, err := publicVerification(legacy); err != nil {
		t.Fatalf("legacy positive report rejected: %v", err)
	}
	if !supportedVerifierVersion(legacy.VerifierVersion) || supportedVerifierVersion("future-unknown") {
		t.Fatal("verifier compatibility changed")
	}
	set.SchemaVersion = 1
	data, _ := json.Marshal(set)
	if _, err := decodeSet(models.RuleSampleSet{FixturesJSON: string(data), Digest: digest(data)}); !errors.Is(err, ErrCorrupt) {
		t.Fatal("negative semantics smuggled into legacy set")
	}
}

func TestNegativeFixtureReportRejectsForgedSuccess(t *testing.T) {
	for _, r := range []crawlrule.FixtureResult{
		{Name: "bad", Kind: "reject", ErrorKind: "execution", Passed: true},
		{Name: "bad", Kind: "reject", ErrorKind: "validation", Passed: true, Rows: 1},
		{Name: "bad", Kind: "empty", ErrorKind: "no_records", Passed: true, Rows: 1},
		{Name: "bad", Kind: "reject", ErrorKind: "http_status", StatusCode: 403, Passed: true, Rows: 1},
	} {
		report := crawlrule.SummarizeFixtures([]crawlrule.FixtureResult{{Name: "positive", Passed: true, Rows: 1}, r})
		b, _ := json.Marshal(report)
		if _, err := publicVerification(models.RuleVerification{VerifierVersion: VerifierVersion, Passed: true, ReportJSON: string(b)}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("forged success accepted: %+v", r)
		}
	}
	report := crawlrule.SummarizeFixtures([]crawlrule.FixtureResult{{Name: "negative", Kind: "empty", ErrorKind: "no_records", Passed: true}})
	b, _ := json.Marshal(report)
	if _, err := publicVerification(models.RuleVerification{VerifierVersion: VerifierVersion, Passed: false, ReportJSON: string(b)}); err != nil {
		t.Fatalf("diagnostic all-negative report unreadable: %v", err)
	}
	b = []byte(strings.Replace(string(b), `"Passed":false`, `"Passed":true`, 1))
	if _, err := publicVerification(models.RuleVerification{VerifierVersion: VerifierVersion, Passed: true, ReportJSON: string(b)}); !errors.Is(err, ErrCorrupt) {
		t.Fatal("all-negative report authorized publication")
	}
}
