package extract

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func TestValidateRequiredNestedTypesAndUnique(t *testing.T) {
	v, err := NewValidator([]FieldRule{{Pointer: "/id", Kind: "integer", Required: true, Unique: true}, {Pointer: "/title", Kind: "string", Required: true, MinLength: 2}, {Pointer: "/author/url", Kind: "http-url", Required: true}, {Pointer: "/tags/0", Kind: "string", Pattern: "^ok$"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := htmlfetch.DecodeJSON([]byte(`[{"id":9007199254740993,"title":"中文","author":{"url":"https://example.com"},"tags":["ok"]},{"id":9007199254740993,"title":"x","author":{"url":"javascript:bad"},"tags":["bad"]},{"id":1.5,"author":{}}]`), &rows); err != nil {
		t.Fatal(err)
	}
	r := v.Validate(rows, 100)
	if r.InvalidRows != 2 || r.TotalIssues != 7 || r.Truncated {
		t.Fatalf("report: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "javascript") || strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("values leaked: %s", raw)
	}
	r = v.Validate(rows, 1)
	if !r.Truncated || len(r.Issues) != 1 || r.TotalIssues != 7 {
		t.Fatal(r)
	}
}

func TestValidateEscapesNullAndExactNumbers(t *testing.T) {
	v, err := NewValidator([]FieldRule{{Pointer: "/a~1b/~0key", Kind: "number", Required: true, Unique: true}, {Pointer: "/optional", Kind: "string", AllowNull: true}})
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"a/b": map[string]any{"~key": json.Number("1e3")}, "optional": nil}, {"a/b": map[string]any{"~key": json.Number("1000.0")}}}
	r := v.Validate(rows, 10)
	if r.TotalIssues != 1 || r.Issues[0].Code != "duplicate" {
		t.Fatal(r)
	}
	for _, rules := range [][]FieldRule{nil, {{Pointer: "bad", Kind: "string"}}, {{Pointer: "/~9", Kind: "string"}}, {{Kind: "integer", Pattern: "x"}}, {{Kind: "string", Pattern: "["}}} {
		if _, err := NewValidator(rules); err == nil {
			t.Fatalf("bad rules accepted: %+v", rules)
		}
	}
	v, _ = NewValidator([]FieldRule{{Pointer: "/n", Kind: "number"}})
	if r := v.Validate([]map[string]any{{"n": json.Number("1e999999999")}}, 1); r.InvalidRows != 1 {
		t.Fatal("unbounded exponent accepted")
	}
}
