package rulequality

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/extract"
)

func TestSummaryDistinguishesMissingNullEmptyAndZero(t *testing.T) {
	rule := crawlrule.Rule{Format: "json", Fields: []crawlrule.Field{{Name: "value", Validation: crawlrule.Validation{Kind: "any"}}}}
	result := crawlrule.Result{Records: []map[string]any{{}, {"value": nil}, {"value": ""}, {"value": "  "}, {"value": json.Number("0")}, {"value": false}}, Validation: extract.Report{Rows: 6, InvalidRows: 1, TotalIssues: 1, Issues: []extract.Issue{{Row: 1, Pointer: "/value", Code: "null"}}}}
	fields, incomplete := Summarize(rule, result)
	if incomplete || len(fields) != 1 {
		t.Fatalf("summary = %+v, incomplete=%v", fields, incomplete)
	}
	got := fields[0]
	if got.Rows != 6 || got.Present != 5 || got.Null != 1 || got.NonEmpty != 2 || got.Invalid != 1 {
		t.Fatalf("wrong distinctions: %+v", got)
	}
}

func TestSummaryCountsEachInvalidFieldRowOnlyOnce(t *testing.T) {
	rule := crawlrule.Rule{Format: "json", Fields: []crawlrule.Field{{Name: "a/b~c", Validation: crawlrule.Validation{Kind: "string"}}}}
	result := crawlrule.Result{Records: []map[string]any{{"a/b~c": ""}}, Validation: extract.Report{Rows: 1, InvalidRows: 1, TotalIssues: 2, Issues: []extract.Issue{{Row: 0, Pointer: "/a~1b~0c", Code: "too-short"}, {Row: 0, Pointer: "/a~1b~0c", Code: "pattern"}}}}
	fields, incomplete := Summarize(rule, result)
	if incomplete || fields[0].Invalid != 1 {
		t.Fatalf("fields=%+v incomplete=%v", fields, incomplete)
	}
}

func TestSummaryIncompleteAndBounded(t *testing.T) {
	rule := crawlrule.Rule{Format: "json"}
	for i := 0; i < MaxFields+1; i++ {
		rule.Fields = append(rule.Fields, crawlrule.Field{Name: fmt.Sprintf("f%02d", i)})
	}
	fields, incomplete := Summarize(rule, crawlrule.Result{Validation: extract.Report{Truncated: true}})
	if !incomplete || len(fields) != MaxFields {
		t.Fatalf("fields=%d incomplete=%v", len(fields), incomplete)
	}
	_, incomplete = Summarize(crawlrule.Rule{Fields: rule.Fields[:1]}, crawlrule.Result{Records: []map[string]any{{}}, Validation: extract.Report{Rows: 0}})
	if !incomplete {
		t.Fatal("unevaluated validation must be incomplete")
	}
}

func TestSummarySchemaDigestIgnoresExtractionLocationAndOrder(t *testing.T) {
	rule := crawlrule.Rule{Format: "html", Fields: []crawlrule.Field{{Name: "title", Selector: "h1"}, {Name: "url", Attribute: "href", Transforms: []crawlrule.Transform{{Op: "absolute_url"}}}}}
	before := FieldSchemaDigest(rule)
	rule.Fields[0].Selector = "h2"
	rule.Fields[0], rule.Fields[1] = rule.Fields[1], rule.Fields[0]
	if before != FieldSchemaDigest(rule) {
		t.Fatal("selectors and declaration order are not field schema")
	}
	rule.Fields[0].Validation.Required = true
	if before == FieldSchemaDigest(rule) {
		t.Fatal("validation contract changes must change schema digest")
	}
}
