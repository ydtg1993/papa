package crawlrule_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestEvidenceBudgetIncludesMissingFieldsAndExactLimit(t *testing.T) {
	for _, format := range []string{"html", "json"} {
		t.Run(format, func(t *testing.T) {
			rule := htmlRule()
			body := `<article><h2>A</h2></article><article><h2>B</h2></article>`
			missing := crawlrule.Field{Name: "missing", Selector: ".absent"}
			if format == "json" {
				rule = jsonRule()
				body = `{"items":[{"title":"A"},{"title":"B"}]}`
				missing = crawlrule.Field{Name: "missing", Pointer: "/absent"}
			}
			rule.Fields = append([]crawlrule.Field{missing}, rule.Fields...)
			for _, limit := range []int{1, 4} {
				rule.MaxEvidence = limit
				out, err := compile(t, rule).Evaluate(response(body))
				if err != nil || len(out.Records) != 2 || out.Validation.Rows != 2 || out.Truncated || out.Validation.Truncated {
					t.Fatalf("evidence limit changed acceptance: %+v, %v", out, err)
				}
				if len(out.Evidence) != limit || out.EvidenceTruncated != (limit < 4) {
					t.Fatalf("evidence limit %d: %+v", limit, out)
				}
				if out.Evidence[0].Field != "missing" || out.Evidence[0].Present {
					t.Fatalf("missing field not included in budget: %+v", out.Evidence[0])
				}
			}
		})
	}
}

func TestEvidenceBudgetDoesNotSkipTransformsOrValidation(t *testing.T) {
	rule := jsonRule()
	rule.MaxEvidence = 1
	rule.Fields = []crawlrule.Field{
		{Name: "missing", Pointer: "/missing"},
		{Name: "value", Pointer: "/value", Transforms: []crawlrule.Transform{{Op: "number"}}, Validation: crawlrule.Validation{Kind: "integer", Required: true}},
	}
	compiled := compile(t, rule)
	out, err := compiled.Evaluate(response(`{"items":[{"value":"1"},{"value":"9007199254740993"}]}`))
	if err != nil || len(out.Evidence) != 1 || !out.EvidenceTruncated || out.Validation.Rows != 2 || out.Records[0]["value"] != json.Number("1") || out.Records[1]["value"] != json.Number("9007199254740993") {
		t.Fatalf("transform beyond evidence cap skipped: %+v, %v", out, err)
	}
	for _, tc := range []struct {
		body string
		want error
	}{
		{`{"items":[{"value":"1"},{"value":"not-number"}]}`, crawlrule.ErrTransform},
		{`{"items":[{"value":"1"},{}]}`, crawlrule.ErrValidation},
		{`{"items":[{"value":"1"},{"value":"2.5"}]}`, crawlrule.ErrValidation},
	} {
		out, err := compiled.Evaluate(response(tc.body))
		if !errors.Is(err, tc.want) || len(out.Evidence) != 1 || !out.EvidenceTruncated {
			t.Fatalf("post-cap failure lost: %+v, %v; want %v", out, err, tc.want)
		}
	}
}

func TestEvidenceBudgetDefaultsAndCompileLimits(t *testing.T) {
	rule := jsonRule()
	if got := compile(t, rule).Rule().MaxEvidence; got != crawlrule.DefaultMaxEvidence {
		t.Fatalf("zero default = %d", got)
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "max_evidence") {
		t.Fatal("zero evidence budget should be omitted from JSON")
	}
	parsed, err := crawlrule.Parse(raw)
	if err != nil || parsed.Rule().MaxEvidence != crawlrule.DefaultMaxEvidence {
		t.Fatalf("omitted JSON evidence default failed: %v", err)
	}
	for _, value := range []int{-1, crawlrule.MaxEvidenceLimit + 1} {
		rule.MaxEvidence = value
		if _, err := crawlrule.Compile(rule); err == nil {
			t.Fatalf("invalid evidence budget %d accepted", value)
		}
	}
	rule.MaxEvidence = crawlrule.MaxEvidenceLimit
	compiled := compile(t, rule)
	raw, err = json.Marshal(compiled.Rule())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = crawlrule.Parse(raw)
	if err != nil || parsed.Rule().MaxEvidence != crawlrule.MaxEvidenceLimit {
		t.Fatalf("explicit evidence limit round trip failed: %v", err)
	}
}

func TestEvidenceBudgetDoesNotRejectFixtureActivation(t *testing.T) {
	rule := jsonRule()
	rule.MaxEvidence = 1
	fixture := crawlrule.Fixture{
		Name:     "two rows exceed provenance budget",
		Response: response(`{"items":[{"title":"A"},{"title":"B"}]}`),
		Expect:   crawlrule.Expectations{RowCount: 2, Fields: []crawlrule.FieldExpectation{{Row: 1, Pointer: "/title", Value: "B"}}},
	}
	registry := crawlrule.NewRegistry()
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	if report, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{fixture}); err != nil || !report.Passed {
		t.Fatalf("informational evidence truncation blocked activation: %+v, %v", report, err)
	}
	compiled, err := registry.Verified(rule.ID, rule.Version)
	if err != nil {
		t.Fatal(err)
	}
	out, err := compiled.Evaluate(fixture.Response)
	if err != nil || !out.EvidenceTruncated || len(out.Evidence) != 1 || len(out.Records) != 2 {
		t.Fatalf("verified rule lost evidence budget: %+v, %v", out, err)
	}
}

func TestEvidenceBudgetWithManyAbsentFieldsAndRows(t *testing.T) {
	rule := jsonRule()
	rule.MaxEvidence = 7
	rule.Fields = nil
	for i := 0; i < crawlrule.MaxFieldsLimit; i++ {
		rule.Fields = append(rule.Fields, crawlrule.Field{Name: fmt.Sprintf("missing%d", i), Pointer: "/absent"})
	}
	body := `{"items":[` + strings.TrimSuffix(strings.Repeat(`{},`, 200), ",") + `]}`
	out, err := compile(t, rule).Evaluate(response(body))
	if !errors.Is(err, crawlrule.ErrNoRecords) || len(out.Records) != 200 || len(out.Evidence) != 7 || !out.EvidenceTruncated || out.Validation.Rows != 200 {
		t.Fatalf("sparse records exceeded evidence cap or were not processed: rows=%d evidence=%d err=%v", len(out.Records), len(out.Evidence), err)
	}
	for _, record := range out.Records {
		if len(record) != 0 {
			t.Fatal("missing fields added to output records")
		}
	}
}
