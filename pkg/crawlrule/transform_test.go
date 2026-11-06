package crawlrule_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestTransformsHTMLAndJSONBeforeValidation(t *testing.T) {
	want := map[string]any{
		"id": json.Number("9007199254740993"), "enabled": true,
		"url": "https://example.test/detail/1", "date": "2026-11-06T00:00:00Z",
	}
	for _, format := range []string{"html", "json"} {
		t.Run(format, func(t *testing.T) {
			rule := htmlRule()
			rule.Format = format
			rule.Fields = []crawlrule.Field{
				{Name: "id", Transforms: []crawlrule.Transform{{Op: "trim"}, {Op: "number"}}, Validation: crawlrule.Validation{Kind: "integer", Required: true}},
				{Name: "enabled", Transforms: []crawlrule.Transform{{Op: "bool"}}, Validation: crawlrule.Validation{Required: true}},
				{Name: "url", Transforms: []crawlrule.Transform{{Op: "absolute_url"}}, Validation: crawlrule.Validation{Required: true}},
				{Name: "date", Transforms: []crawlrule.Transform{{Op: "date", Layout: "2006/01/02"}}, Validation: crawlrule.Validation{Required: true}},
				{Name: "absent", Transforms: []crawlrule.Transform{{Op: "number"}}},
			}
			body := `<base href="https://untrusted.test/"><article id=" 9007199254740993 " enabled="true" url="../detail/1#fragment" date="2026/11/06"></article>`
			for i := range rule.Fields {
				if format == "html" {
					rule.Fields[i].Attribute = rule.Fields[i].Name
				} else {
					rule.Fields[i].Pointer = "/" + rule.Fields[i].Name
				}
			}
			if format == "json" {
				rule.Rows = "/items"
				body = `{"items":[{"id":" 9007199254740993 ","enabled":"true","url":"../detail/1#fragment","date":"2026/11/06"}]}`
			}
			compiled := compile(t, rule)
			out, err := compiled.Evaluate(response(body))
			if err != nil || len(out.Records) != 1 || !reflect.DeepEqual(out.Records[0], want) {
				t.Fatalf("transforms = %#v, err = %v", out.Records, err)
			}
			if out.Validation.InvalidRows != 0 || len(out.Evidence) != 5 {
				t.Fatalf("unexpected validation/evidence: %+v", out)
			}
			for i, evidence := range out.Evidence {
				if evidence.Record != 0 || evidence.Field != rule.Fields[i].Name || evidence.Present != (i < 4) || evidence.SourceURL != "https://example.test/catalog?page=1" {
					t.Fatalf("field evidence = %+v", evidence)
				}
				if evidence.Attribute != rule.Fields[i].Attribute || evidence.Pointer != rule.Fields[i].Pointer {
					t.Fatalf("source expression lost: %+v", evidence)
				}
			}
			if !reflect.DeepEqual(out.Evidence[0].Steps, []string{"trim", "number"}) || len(out.Evidence[4].Steps) != 0 {
				t.Fatalf("evidence steps = %+v", out.Evidence)
			}
		})
	}
}

func TestTransformsStrictValuesAndSafeErrors(t *testing.T) {
	tests := []struct {
		name      string
		transform crawlrule.Transform
		value     any
	}{
		{"number whitespace", crawlrule.Transform{Op: "number"}, " 12 "},
		{"number plus", crawlrule.Transform{Op: "number"}, "+12"},
		{"number leading zero", crawlrule.Transform{Op: "number"}, "012"},
		{"number incomplete", crawlrule.Transform{Op: "number"}, "12."},
		{"number NaN", crawlrule.Transform{Op: "number"}, "NaN"},
		{"number infinity", crawlrule.Transform{Op: "number"}, "Inf"},
		{"number overflow", crawlrule.Transform{Op: "number"}, "1e309"},
		{"number bool", crawlrule.Transform{Op: "number"}, true},
		{"number array", crawlrule.Transform{Op: "number"}, []any{1}},
		{"bool case", crawlrule.Transform{Op: "bool"}, "TRUE"},
		{"bool shorthand", crawlrule.Transform{Op: "bool"}, "yes"},
		{"bool numeric string", crawlrule.Transform{Op: "bool"}, "1"},
		{"bool number", crawlrule.Transform{Op: "bool"}, 1},
		{"date invalid", crawlrule.Transform{Op: "date", Layout: "2006-01-02"}, "secret-invalid-date"},
		{"date calendar", crawlrule.Transform{Op: "date", Layout: "2006-01-02"}, "2026-02-30"},
		{"date wrong type", crawlrule.Transform{Op: "date", Layout: "2006-01-02"}, 20261106},
		{"url script", crawlrule.Transform{Op: "absolute_url"}, "javascript:secret-value"},
		{"url credentials", crawlrule.Transform{Op: "absolute_url"}, "https://secret-user:secret-password@example.test/"},
		{"url blank", crawlrule.Transform{Op: "absolute_url"}, "   "},
		{"url wrong type", crawlrule.Transform{Op: "absolute_url"}, map[string]any{"url": "secret-value"}},
		{"trim wrong type", crawlrule.Transform{Op: "trim"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := jsonRule()
			rule.Fields = []crawlrule.Field{{Name: "value", Pointer: "/value", Transforms: []crawlrule.Transform{tt.transform}}}
			body, err := json.Marshal(map[string]any{"items": []any{map[string]any{"value": tt.value}}})
			if err != nil {
				t.Fatal(err)
			}
			out, err := compile(t, rule).Evaluate(response(string(body)))
			if !errors.Is(err, crawlrule.ErrTransform) || !strings.Contains(err.Error(), `row 0 field "value" step 0`) {
				t.Fatalf("result = %+v, err = %v", out, err)
			}
			if strings.Contains(err.Error(), "secret-") {
				t.Fatalf("field value leaked in error: %v", err)
			}
			if len(out.Evidence) != 1 || len(out.Evidence[0].Steps) != 0 {
				t.Fatalf("failed transform counted as successful: %+v", out.Evidence)
			}
		})
	}
}

func TestTransformNativeValuesNullMissingAndUniqueness(t *testing.T) {
	rule := jsonRule()
	rule.Fields = []crawlrule.Field{
		{Name: "number", Pointer: "/number", Transforms: []crawlrule.Transform{{Op: "number"}}},
		{Name: "bool", Pointer: "/bool", Transforms: []crawlrule.Transform{{Op: "bool"}}},
		{Name: "date", Pointer: "/date", Transforms: []crawlrule.Transform{{Op: "date", Layout: time.RFC3339Nano}}},
		{Name: "null", Pointer: "/null", Transforms: []crawlrule.Transform{{Op: "number"}}, Validation: crawlrule.Validation{Required: true, AllowNull: true}},
	}
	out, err := compile(t, rule).Evaluate(response(`{"items":[{"number":9007199254740993,"bool":false,"date":"2026-11-06T08:09:10.123456789+08:00","null":null}]}`))
	if err != nil || out.Records[0]["number"] != json.Number("9007199254740993") || out.Records[0]["bool"] != false || out.Records[0]["date"] != "2026-11-06T00:09:10.123456789Z" {
		t.Fatalf("native/null transforms = %+v, err = %v", out, err)
	}
	if !out.Evidence[3].Present || len(out.Evidence[3].Steps) != 0 {
		t.Fatalf("null evidence = %+v", out.Evidence[3])
	}
	for _, body := range []string{`{"items":[{}]}`, `{"items":[{"null":null}]}`} {
		rule.Fields[3].Validation.AllowNull = false
		if _, err := compile(t, rule).Evaluate(response(body)); !errors.Is(err, crawlrule.ErrValidation) {
			t.Fatalf("missing/null should fail validation, not transform: %v", err)
		}
	}
	rule.Fields = []crawlrule.Field{{Name: "value", Pointer: "/value", Transforms: []crawlrule.Transform{{Op: "number"}}, Validation: crawlrule.Validation{Unique: true}}}
	out, err = compile(t, rule).Evaluate(response(`{"items":[{"value":"1"},{"value":"1.0"}]}`))
	if !errors.Is(err, crawlrule.ErrValidation) || out.Validation.Issues[0].Code != "duplicate" {
		t.Fatalf("uniqueness ran before number conversion: %+v, %v", out, err)
	}
}

func TestDateTransformUTCYearBoundaries(t *testing.T) {
	rule := jsonRule()
	rule.Fields = []crawlrule.Field{{Name: "date", Pointer: "/date", Transforms: []crawlrule.Transform{{Op: "date", Layout: time.RFC3339Nano}}}}
	compiled := compile(t, rule)
	for _, input := range []string{"9999-12-31T23:30:00-01:00", "0000-01-01T00:30:00+01:00"} {
		body, err := json.Marshal(map[string]any{"items": []any{map[string]any{"date": input}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := compiled.Evaluate(response(string(body))); !errors.Is(err, crawlrule.ErrTransform) {
			t.Fatalf("UTC-normalized date outside RFC3339 range was accepted: %s, %v", input, err)
		}
	}
	for _, input := range []string{"0000-01-01T00:00:00Z", "9999-12-31T23:59:59.999999999Z"} {
		body, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"date": input}}})
		out, err := compiled.Evaluate(response(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		value := out.Records[0]["date"].(string)
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil || value != input {
			t.Fatalf("valid RFC3339 boundary did not round-trip: %s, %v", value, err)
		}
	}
}

func TestCompileRejectsInvalidTransformsAndOptions(t *testing.T) {
	for _, transforms := range [][]crawlrule.Transform{
		{{Op: ""}}, {{Op: "javascript"}}, {{Op: " number"}},
		{{Op: "number", Layout: "2006-01-02"}}, {{Op: "date"}},
		{{Op: "date", Layout: "literal"}}, {{Op: "date", Layout: "15:04:05"}},
		{{Op: "date", Layout: time.RFC1123}}, {{Op: "date", Layout: strings.Repeat("x", 257)}},
		make([]crawlrule.Transform, crawlrule.MaxTransformsLimit+1),
	} {
		rule := jsonRule()
		rule.Fields[0].Transforms = transforms
		if _, err := crawlrule.Compile(rule); err == nil {
			t.Fatalf("invalid transforms accepted: %+v", transforms)
		}
	}
	for _, config := range []string{
		`{"op":"number","trim":true}`, `{"op":"date","layout":"2006-01-02","location":"Local"}`,
	} {
		raw := `{"schema_version":1,"id":"x","version":"1","format":"json","fields":[{"name":"x","transforms":[` + config + `]}]}`
		if _, err := crawlrule.Parse([]byte(raw)); err == nil {
			t.Fatalf("unknown transform option accepted: %s", raw)
		}
	}
}

func TestTransformRuleSnapshotsAndConcurrentEvaluation(t *testing.T) {
	rule := jsonRule()
	rule.Fields[0].Transforms = []crawlrule.Transform{{Op: "trim"}, {Op: "number"}}
	rule.Fields[0].Validation.Kind = "integer"
	rule.Fields[0].Validation.MinLength = 0
	compiled := compile(t, rule)
	rule.Fields[0].Transforms[0].Op = "bool"
	snapshot := compiled.Rule()
	snapshot.Fields[0].Transforms[1].Op = "date"
	if got := compiled.Rule().Fields[0].Transforms; got[0].Op != "trim" || got[1].Op != "number" {
		t.Fatalf("caller-owned transform slice retained: %+v", got)
	}
	raw, err := json.Marshal(compiled.Rule())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := crawlrule.Parse(raw)
	if err != nil || !reflect.DeepEqual(reloaded.Rule(), compiled.Rule()) {
		t.Fatalf("transform round trip failed: %v", err)
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 10 {
				out, err := compiled.Evaluate(response(`{"items":[{"title":" 42 "}]}`))
				if err != nil || out.Records[0]["title"] != json.Number("42") || !reflect.DeepEqual(out.Evidence[0].Steps, []string{"trim", "number"}) {
					t.Errorf("concurrent evaluation = %+v, %v", out, err)
					return
				}
				out.Records[0]["title"] = "modified"
				out.Evidence[0].Steps[0] = "modified"
			}
		}()
	}
	workers.Wait()
}

func TestEvaluateRenderedUsesSharedHTMLRulesWithoutHTTPStatus(t *testing.T) {
	rule := htmlRule()
	rule.Fields[0].Transforms = []crawlrule.Transform{{Op: "number"}}
	rule.Fields[0].Validation.Kind = "integer"
	rule.Fields[0].Validation.MinLength = 0
	rule.Next = &crawlrule.Next{Selector: "a.next"}
	body := `<article><h2>42</h2></article><a class="next" href="?page=2">Next</a>`
	compiled := compile(t, rule)
	resp := response(body)
	expected, err := compiled.Evaluate(resp)
	if err != nil {
		t.Fatal(err)
	}
	resp.StatusCode = 403
	if _, err := compiled.Evaluate(resp); err == nil {
		t.Fatal("HTTP evaluation discarded status check")
	}
	out, err := compiled.EvaluateRendered(resp.URL.String(), resp.Body)
	if err != nil || !reflect.DeepEqual(out, expected) {
		t.Fatalf("rendered extraction differs: %+v, %v", out, err)
	}
	if _, err := compile(t, jsonRule()).EvaluateRendered(resp.URL.String(), []byte(`{"items":[{"title":"ok"}]}`)); err == nil {
		t.Fatal("rendered JSON accepted")
	}
	for _, rawURL := range []string{"", "/relative", "file:///tmp/test", "https://user:password@example.test/"} {
		if _, err := compiled.EvaluateRendered(rawURL, resp.Body); err == nil {
			t.Fatalf("invalid final URL accepted: %s", rawURL)
		}
	}
	rule.MaxBodyBytes = len(body) - 1
	if _, err := compile(t, rule).EvaluateRendered(resp.URL.String(), resp.Body); !errors.Is(err, crawlrule.ErrBodyTooLarge) {
		t.Fatalf("rendered body budget ignored: %v", err)
	}
	var nilRule *crawlrule.Compiled
	if _, err := nilRule.EvaluateRendered(resp.URL.String(), resp.Body); err == nil {
		t.Fatal("nil compiled rule accepted")
	}
}
