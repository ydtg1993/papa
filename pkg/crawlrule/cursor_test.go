package crawlrule_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
)

func cursorRule() crawlrule.Rule {
	rule := jsonRule()
	rule.Next = &crawlrule.Next{Mode: "cursor", Pointer: "/pagination/next"}
	return rule
}

func TestJSONRootNextCursorRemainsOpaque(t *testing.T) {
	compiled := compile(t, cursorRule())
	for _, cursor := range []string{
		"../page2?offset=10#part", "https://outside.test/page", "javascript:not-a-url",
		"游标=ab+/==", "  token with spaces \t", "   ", strings.Repeat("x", crawlrule.MaxNextCursorBytes),
	} {
		body, err := json.Marshal(map[string]any{
			"items":      []any{map[string]any{"title": "A"}},
			"pagination": map[string]any{"next": cursor},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := compiled.Evaluate(response(string(body)))
		if err != nil || out.NextCursor != cursor || out.NextURL != "" || !reflect.DeepEqual(out.Records, []map[string]any{{"title": "A"}}) {
			t.Fatalf("root cursor changed records or URL semantics: cursor bytes=%d result=%+v err=%v", len(cursor), out, err)
		}
	}
}

func TestJSONNextCursorEmptyNullMissingAndWrongTypes(t *testing.T) {
	compiled := compile(t, cursorRule())
	for _, pagination := range []string{``, `,"pagination":{}`, `,"pagination":{"next":null}`, `,"pagination":{"next":""}`} {
		out, err := compiled.Evaluate(response(`{"items":[{"title":"A"}]` + pagination + `}`))
		if err != nil || out.NextCursor != "" || out.NextURL != "" {
			t.Fatalf("terminal cursor rejected: %+v, %v", out, err)
		}
	}
	for _, value := range []string{`1`, `false`, `[]`, `{}`, `"` + strings.Repeat("x", crawlrule.MaxNextCursorBytes+1) + `"`, `"` + strings.Repeat("中", crawlrule.MaxNextCursorBytes/3+1) + `"`} {
		out, err := compiled.Evaluate(response(`{"items":[{"title":"A"}],"pagination":{"next":` + value + `}}`))
		if !errors.Is(err, crawlrule.ErrNextCursor) || out.NextCursor != "" || out.NextURL != "" {
			t.Fatalf("invalid cursor accepted: bytes=%d result=%+v err=%v", len(value), out, err)
		}
	}
}

func TestCompileNextModesAndSchemaRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule crawlrule.Rule
	}{
		{"unknown mode", func() crawlrule.Rule { r := cursorRule(); r.Next.Mode = "opaque"; return r }()},
		{"whitespace mode", func() crawlrule.Rule { r := cursorRule(); r.Next.Mode = " cursor "; return r }()},
		{"HTML cursor", func() crawlrule.Rule {
			r := htmlRule()
			r.Next = &crawlrule.Next{Mode: "cursor", Selector: "a"}
			return r
		}()},
		{"external cursor", func() crawlrule.Rule { r := cursorRule(); r.Next.AllowExternal = true; return r }()},
		{"cursor selector", func() crawlrule.Rule { r := cursorRule(); r.Next.Selector = "a"; return r }()},
		{"cursor attribute", func() crawlrule.Rule { r := cursorRule(); r.Next.Attribute = "href"; return r }()},
		{"malformed pointer", func() crawlrule.Rule { r := cursorRule(); r.Next.Pointer = "/~invalid"; return r }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := crawlrule.Compile(tc.rule); err == nil {
				t.Fatal("invalid next configuration accepted")
			}
		})
	}
	rule := cursorRule()
	compiled := compile(t, rule)
	rule.Next.Mode = "url"
	snapshot := compiled.Rule()
	snapshot.Next.Mode = "url"
	if compiled.Rule().Next.Mode != "cursor" {
		t.Fatal("next mode retained caller-owned pointer")
	}
	raw, err := json.Marshal(compiled.Rule())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := crawlrule.Parse(raw)
	if err != nil || !reflect.DeepEqual(parsed.Rule(), compiled.Rule()) {
		t.Fatalf("cursor schema round trip failed: %v", err)
	}
	for _, format := range []string{"html", "json"} {
		rule := htmlRule()
		rule.Next = &crawlrule.Next{Selector: "a"}
		if format == "json" {
			rule = jsonRule()
			rule.Next = &crawlrule.Next{Pointer: "/next"}
		}
		if compile(t, rule).Rule().Next.Mode != "url" {
			t.Fatalf("%s legacy next did not default to URL mode", format)
		}
	}
}

func TestCursorFixtureRequiresExactExpectedCursorBeforeActivation(t *testing.T) {
	rule := cursorRule()
	registry := crawlrule.NewRegistry()
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	fixture := crawlrule.Fixture{
		Name:     "root cursor",
		Response: response(`{"items":[{"title":"A"}],"pagination":{"next":"private-opaque-cursor"}}`),
		Expect:   crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "A"}}},
	}
	for _, expected := range []string{"", "wrong"} {
		fixture.Expect.NextCursor = expected
		report, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{fixture})
		if !errors.Is(err, crawlrule.ErrFixtureFailed) || report.Passed || len(report.Results[0].Failures) != 1 || report.Results[0].Failures[0] != "next cursor mismatch" {
			t.Fatalf("incorrect cursor fixture accepted or leaked value: %+v, %v", report, err)
		}
		if _, err := registry.Active(rule.ID); !errors.Is(err, crawlrule.ErrNoActiveRule) {
			t.Fatalf("failed fixture activated rule: %v", err)
		}
		if _, err := registry.Verified(rule.ID, rule.Version); !errors.Is(err, crawlrule.ErrUnverifiedRule) {
			t.Fatalf("failed fixture verified rule: %v", err)
		}
	}
	fixture.Expect.NextCursor = "private-opaque-cursor"
	if report, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{fixture}); err != nil || !report.Passed {
		t.Fatalf("matching cursor fixture rejected: %+v, %v", report, err)
	}
}
