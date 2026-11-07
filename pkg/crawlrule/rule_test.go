package crawlrule_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func response(body string) *htmlfetch.Response {
	u, _ := url.Parse("https://example.test/catalog?page=1")
	return &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(body)}
}

func htmlRule() crawlrule.Rule {
	return crawlrule.Rule{
		SchemaVersion: 1, ID: "catalog", Version: "v1", Format: "html", Rows: "article",
		Fields: []crawlrule.Field{{Name: "title", Selector: "h2", Validation: crawlrule.Validation{Required: true, MinLength: 1}}},
	}
}

func jsonRule() crawlrule.Rule {
	return crawlrule.Rule{
		SchemaVersion: 1, ID: "catalog", Version: "v1", Format: "json", Rows: "/items",
		Fields: []crawlrule.Field{{Name: "title", Pointer: "/title", Validation: crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}}},
	}
}

func compile(t *testing.T, rule crawlrule.Rule) *crawlrule.Compiled {
	t.Helper()
	c, err := crawlrule.Compile(rule)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHTMLRowsTextAttributesAndNext(t *testing.T) {
	rule := htmlRule()
	rule.Fields = append(rule.Fields,
		crawlrule.Field{Name: "id", Attribute: "data-id", Validation: crawlrule.Validation{Required: true, Unique: true}},
		crawlrule.Field{Name: "href", Selector: "a", Attribute: "href"},
		crawlrule.Field{Name: "absent", Selector: ".absent"},
	)
	rule.Next = &crawlrule.Next{Selector: "a[rel=next]"}
	c := compile(t, rule)
	out, err := c.Evaluate(response(`<base href="https://other.test/"><article data-id="1"><h2> First <em>title</em> </h2><h2>Ignored</h2><a href="/1">open</a></article><article data-id="2"><h2>Second</h2></article><a rel="next" href="?page=2#top">next</a>`))
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"title": "First title", "id": "1", "href": "/1"}, {"title": "Second", "id": "2"}}
	if !reflect.DeepEqual(out.Records, want) || out.NextURL != "https://example.test/catalog?page=2" || out.Truncated || out.Validation.InvalidRows != 0 {
		t.Fatalf("unexpected result: %+v", out)
	}
	// JSON round trips retain the selected defaults and all validations.
	raw, err := json.Marshal(c.Rule())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := crawlrule.Parse(raw)
	if err != nil || !reflect.DeepEqual(reloaded.Rule(), c.Rule()) {
		t.Fatalf("rule round trip failed: %s, %v", raw, err)
	}
}

func TestJSONPointersEscapesRootTypesAndExactNumbers(t *testing.T) {
	rule := jsonRule()
	rule.Rows = "/payload/items"
	rule.Fields = []crawlrule.Field{
		{Name: "id/~", Pointer: "/a~1b/~0key/0", Validation: crawlrule.Validation{Kind: "integer", Required: true}},
		{Name: "object", Pointer: "/nested", Validation: crawlrule.Validation{Kind: "object", Required: true}},
		{Name: "flag", Pointer: "/yes", Validation: crawlrule.Validation{Kind: "bool"}},
		{Name: "nothing", Pointer: "/null", Validation: crawlrule.Validation{AllowNull: true}},
		{Name: "root", Pointer: ""},
		{Name: "leading-zero", Pointer: "/a~1b/~0key/00"},
		{Name: "append-token", Pointer: "/a~1b/~0key/-"},
	}
	rule.Next = &crawlrule.Next{Pointer: "/links/next"}
	out, err := compile(t, rule).Evaluate(response(`{"payload":{"items":[{"a/b":{"~key":[9007199254740993]},"nested":{"v":1e3},"yes":true,"null":null}]},"links":{"next":"/page/2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	row := out.Records[0]
	if row["id/~"] != json.Number("9007199254740993") || row["flag"] != true || row["nothing"] != nil || len(row) != 5 {
		t.Fatalf("types or escaped pointers lost: %#v", row)
	}
	if row["object"].(map[string]any)["v"] != json.Number("1e3") || out.NextURL != "https://example.test/page/2" {
		t.Fatalf("unexpected result: %+v", out)
	}
	// Root rows and scalar rows are supported with an empty field pointer.
	rule.Rows = ""
	rule.Next = nil
	rule.Fields = []crawlrule.Field{{Name: "value", Validation: crawlrule.Validation{Kind: "integer", Required: true}}}
	out, err = compile(t, rule).Evaluate(response(`[1,2]`))
	if err != nil || out.Records[1]["value"] != json.Number("2") {
		t.Fatalf("root array failed: %+v, %v", out, err)
	}
}

func TestCompileRejectsMalformedRules(t *testing.T) {
	tests := []struct {
		name string
		edit func(*crawlrule.Rule)
	}{
		{"schema", func(r *crawlrule.Rule) { r.SchemaVersion = 2 }},
		{"id", func(r *crawlrule.Rule) { r.ID = " " }},
		{"version", func(r *crawlrule.Rule) { r.Version = "" }},
		{"format", func(r *crawlrule.Rule) { r.Format = "javascript" }},
		{"rows selector", func(r *crawlrule.Rule) { r.Rows = "[" }},
		{"empty selector", func(r *crawlrule.Rule) { r.Rows = "" }},
		{"field selector", func(r *crawlrule.Rule) { r.Fields[0].Selector = "[" }},
		{"HTML pointer", func(r *crawlrule.Rule) { r.Fields[0].Pointer = "/title" }},
		{"no fields", func(r *crawlrule.Rule) { r.Fields = nil }},
		{"duplicate fields", func(r *crawlrule.Rule) { r.Fields = append(r.Fields, r.Fields[0]) }},
		{"blank name", func(r *crawlrule.Rule) { r.Fields[0].Name = "" }},
		{"invalid kind", func(r *crawlrule.Rule) { r.Fields[0].Validation.Kind = "date" }},
		{"pattern", func(r *crawlrule.Rule) { r.Fields[0].Validation.Pattern = "[" }},
		{"negative rows", func(r *crawlrule.Rule) { r.MaxRows = -1 }},
		{"huge rows", func(r *crawlrule.Rule) { r.MaxRows = crawlrule.MaxRowsLimit + 1 }},
		{"huge body", func(r *crawlrule.Rule) { r.MaxBodyBytes = crawlrule.MaxBodyBytesLimit + 1 }},
		{"huge issues", func(r *crawlrule.Rule) { r.MaxIssues = crawlrule.MaxIssuesLimit + 1 }},
		{"next selector", func(r *crawlrule.Rule) { r.Next = &crawlrule.Next{Selector: "["} }},
		{"next missing selector", func(r *crawlrule.Rule) { r.Next = &crawlrule.Next{} }},
		{"JSON rows pointer", func(r *crawlrule.Rule) { *r = jsonRule(); r.Rows = "/bad~2" }},
		{"JSON field pointer", func(r *crawlrule.Rule) { *r = jsonRule(); r.Fields[0].Pointer = "/bad~" }},
		{"JSON path", func(r *crawlrule.Rule) { *r = jsonRule(); r.Fields[0].Pointer = "$.title" }},
		{"JSON field selector", func(r *crawlrule.Rule) { *r = jsonRule(); r.Fields[0].Selector = "h2" }},
		{"JSON next selector", func(r *crawlrule.Rule) { *r = jsonRule(); r.Next = &crawlrule.Next{Selector: "a"} }},
		{"JSON next pointer", func(r *crawlrule.Rule) { *r = jsonRule(); r.Next = &crawlrule.Next{Pointer: "/~x"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := htmlRule()
			tt.edit(&rule)
			if _, err := crawlrule.Compile(rule); err == nil {
				t.Fatal("invalid rule compiled")
			}
		})
	}
	for _, raw := range []string{
		`null`, `{`, `{} {}`, `{"schema_version":1,"unexpected":true}`,
		`{"schema_version":1,"id":"a","version":"1","format":"json","fields":[{"name":"x","unrecognized":true}]}`,
	} {
		if _, err := crawlrule.Parse([]byte(raw)); err == nil {
			t.Fatalf("invalid JSON schema accepted: %s", raw)
		}
	}
	if _, err := crawlrule.Parse([]byte(strings.Repeat(" ", crawlrule.MaxRuleBytes+1))); err == nil {
		t.Fatal("oversize rule accepted")
	}
}

func TestEvaluateRejectsMalformedAndEmptyResponses(t *testing.T) {
	tests := []struct {
		name string
		rule crawlrule.Rule
		resp *htmlfetch.Response
	}{
		{"nil", htmlRule(), nil},
		{"nil URL", htmlRule(), &htmlfetch.Response{StatusCode: 200}},
		{"empty HTML", htmlRule(), response("")},
		{"HTML no rows", htmlRule(), response("<p>Login first</p>")},
		{"HTML missing field", htmlRule(), response("<article></article>")},
		{"JSON malformed", jsonRule(), response(`{"items":[`)},
		{"JSON trailing", jsonRule(), response(`{"items":[]} {}`)},
		{"JSON missing rows", jsonRule(), response(`{"other":[]}`)},
		{"JSON wrong rows type", jsonRule(), response(`{"items":{}}`)},
		{"JSON empty rows", jsonRule(), response(`{"items":[]}`)},
		{"JSON missing field", jsonRule(), response(`{"items":[{}]}`)},
		{"JSON wrong field type", jsonRule(), response(`{"items":[{"title":1}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := compile(t, tt.rule).Evaluate(tt.resp); err == nil {
				t.Fatal("invalid/empty response returned success")
			}
		})
	}
	for _, status := range []int{0, 301, 403, 500} {
		r := response("<article><h2>Title</h2></article>")
		r.StatusCode = status
		if _, err := compile(t, htmlRule()).Evaluate(r); err == nil {
			t.Fatalf("status %d accepted", status)
		}
	}
	r := htmlRule()
	r.Fields[0].Validation = crawlrule.Validation{}
	if _, err := compile(t, r).Evaluate(response("<article></article>")); !errors.Is(err, crawlrule.ErrNoRecords) {
		t.Fatalf("wholly empty optional record accepted: %v", err)
	}
}

func TestNextURLScopeAndResolution(t *testing.T) {
	tests := []struct {
		name     string
		link     any
		external bool
		want     string
		wantErr  bool
	}{
		{"relative", "?page=2#anchor", false, "https://example.test/catalog?page=2", false},
		{"default port", "https://EXAMPLE.test:443/2", false, "https://example.test/2", false},
		{"host", "https://other.test/2", false, "", true},
		{"scheme", "http://example.test/2", false, "", true},
		{"port", "https://example.test:444/2", false, "", true},
		{"allow external", "//other.test/2", true, "https://other.test/2", false},
		{"javascript", "javascript:alert(1)", true, "", true},
		{"userinfo", "https://user:password@example.test/2", true, "", true},
		{"loop", "#top", false, "", true},
		{"invalid", "%zz", false, "", true},
		{"type", 123, false, "", true},
		{"null", nil, false, "", false},
		{"blank", "   ", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := jsonRule()
			r.Next = &crawlrule.Next{Pointer: "/next", AllowExternal: tt.external}
			raw, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"title": "A"}}, "next": tt.link})
			out, err := compile(t, r).Evaluate(response(string(raw)))
			if (err != nil) != tt.wantErr || out.NextURL != tt.want || (tt.wantErr && !errors.Is(err, crawlrule.ErrNextURL)) {
				t.Fatalf("next = %q, err = %v", out.NextURL, err)
			}
		})
	}
}

func TestRowBodyAndIssueBudgets(t *testing.T) {
	for _, format := range []string{"html", "json"} {
		t.Run(format, func(t *testing.T) {
			r, body := htmlRule(), `<article><h2>A</h2></article><article><h2>B</h2></article>`
			if format == "json" {
				r, body = jsonRule(), `{"items":[{"title":"A"},{"title":"B"}]}`
			}
			r.MaxRows = 1
			out, err := compile(t, r).Evaluate(response(body))
			if err != nil || len(out.Records) != 1 || !out.Truncated || out.Validation.Rows != 1 {
				t.Fatalf("row budget failed: %+v, %v", out, err)
			}
			r.MaxRows = 2
			r.MaxBodyBytes = len(body)
			out, err = compile(t, r).Evaluate(response(body))
			if err != nil || out.Truncated {
				t.Fatalf("exact budget failed: %+v, %v", out, err)
			}
			r.MaxBodyBytes--
			out, err = compile(t, r).Evaluate(response(body))
			if !errors.Is(err, crawlrule.ErrBodyTooLarge) || len(out.Records) != 0 {
				t.Fatalf("body prefix was parsed: %+v, %v", out, err)
			}
		})
	}
	r := jsonRule()
	r.MaxIssues = 1
	out, err := compile(t, r).Evaluate(response(`{"items":[{},{}]}`))
	if !errors.Is(err, crawlrule.ErrValidation) || out.Validation.InvalidRows != 2 || out.Validation.TotalIssues != 2 || len(out.Validation.Issues) != 1 || !out.Validation.Truncated || out.Truncated {
		t.Fatalf("issue budget/report failed: %+v, %v", out, err)
	}
	r = jsonRule()
	r.Fields[0].Validation.Unique = true
	out, err = compile(t, r).Evaluate(response(`{"items":[{"title":"same"},{"title":"same"}]}`))
	if !errors.Is(err, crawlrule.ErrValidation) || out.Validation.Issues[0].Code != "duplicate" {
		t.Fatalf("batch validation not applied: %+v, %v", out, err)
	}
}
