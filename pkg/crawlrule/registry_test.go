package crawlrule_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func fixture(title string) crawlrule.Fixture {
	return crawlrule.Fixture{
		Name: "catalog-page", Response: response(`<article><h2>` + title + `</h2></article>`),
		Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: title}}},
	}
}

func TestRegistryActivationRequiresPassingExplicitFixtures(t *testing.T) {
	var registry crawlrule.Registry // zero value is useful
	rule := htmlRule()
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Active(rule.ID); !errors.Is(err, crawlrule.ErrNoActiveRule) {
		t.Fatalf("registration unexpectedly activated: %v", err)
	}
	if _, err := registry.Activate(rule.ID, rule.Version, nil); !errors.Is(err, crawlrule.ErrNoFixtures) {
		t.Fatalf("zero fixtures allowed: %v", err)
	}
	good := fixture("A")
	second := fixture("B")
	second.Name = "second-page"
	report, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{good, second})
	if err != nil || !report.Passed || len(report.Results) != 2 {
		t.Fatalf("valid fixture activation failed: %+v, %v", report, err)
	}
	rule.Version = "v2"
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*crawlrule.Fixture)
	}{
		{"no expectations", func(f *crawlrule.Fixture) { f.Expect = crawlrule.Expectations{} }},
		{"no fields", func(f *crawlrule.Fixture) { f.Expect.Fields = nil }},
		{"wrong count", func(f *crawlrule.Fixture) { f.Expect.RowCount = 2 }},
		{"wrong value", func(f *crawlrule.Fixture) { f.Expect.Fields[0].Value = "wrong" }},
		{"missing null", func(f *crawlrule.Fixture) { f.Expect.Fields[0].Pointer = "/absent"; f.Expect.Fields[0].Value = nil }},
		{"wrong pointer", func(f *crawlrule.Fixture) { f.Expect.Fields[0].Pointer = "title" }},
		{"wrong row", func(f *crawlrule.Fixture) { f.Expect.Fields[0].Row = 1 }},
		{"not serializable", func(f *crawlrule.Fixture) { f.Expect.Fields[0].Value = make(chan int) }},
		{"wrong next URL", func(f *crawlrule.Fixture) { f.Expect.NextURL = "https://example.test/next" }},
		{"wrong truncated", func(f *crawlrule.Fixture) { f.Expect.Truncated = true }},
		{"no name", func(f *crawlrule.Fixture) { f.Name = "" }},
		{"invalid response", func(f *crawlrule.Fixture) { f.Response.Body = []byte("<p>login</p>") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fixture("A")
			tt.edit(&bad)
			report, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{second, bad})
			if !errors.Is(err, crawlrule.ErrFixtureFailed) || report.Passed || len(report.Results) != 2 || !report.Results[0].Passed || report.Results[1].Passed {
				t.Fatalf("failed fixture accepted: %+v, %v", report, err)
			}
			active, err := registry.Active(rule.ID)
			if err != nil || active.Rule().Version != "v1" {
				t.Fatalf("failed activation replaced active rule: %v", err)
			}
			if _, err := registry.Rollback(rule.ID); !errors.Is(err, crawlrule.ErrNoRollback) {
				t.Fatalf("failed activation polluted history: %v", err)
			}
		})
	}
	if _, err := registry.Activate(rule.ID, rule.Version, []crawlrule.Fixture{good, good}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("ambiguous fixture names accepted: %v", err)
	}
}

func TestRegistryConflictRollbackAndMutationIsolation(t *testing.T) {
	registry := crawlrule.NewRegistry()
	rule := htmlRule()
	rule.Next = &crawlrule.Next{Selector: "a[rel=next]"}
	if err := registry.Register(rule); err != nil {
		t.Fatal(err)
	}
	// Registration owns both the field slice and next pointer.
	rule.Fields[0].Selector = ".changed"
	rule.Next.Selector = ".changed"
	if err := registry.Register(rule); !errors.Is(err, crawlrule.ErrVersionConflict) {
		t.Fatalf("same-version overwrite allowed: %v", err)
	}
	c, err := registry.Get("catalog", "v1")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := c.Rule()
	snapshot.Fields[0].Selector = ".mutated-copy"
	snapshot.Next.Selector = ".mutated-copy"
	if _, err := registry.Activate("catalog", "v1", []crawlrule.Fixture{fixture("A")}); err != nil {
		t.Fatalf("caller mutation affected registered rule: %v", err)
	}
	// Calling Activate twice for the current version must not create a fake rollback.
	if _, err := registry.Activate("catalog", "v1", []crawlrule.Fixture{fixture("A")}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Rollback("catalog"); !errors.Is(err, crawlrule.ErrNoRollback) {
		t.Fatalf("duplicate activation created history: %v", err)
	}
	for _, version := range []string{"v2", "v3"} {
		r := htmlRule()
		r.Version = version
		if err := registry.Register(r); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Activate("catalog", version, []crawlrule.Fixture{fixture("A")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"v2", "v1"} {
		rolled, err := registry.Rollback("catalog")
		if err != nil || rolled.Rule().Version != want {
			t.Fatalf("rollback want %s: %v", want, err)
		}
	}
	if _, err := registry.Rollback("catalog"); !errors.Is(err, crawlrule.ErrNoRollback) {
		t.Fatalf("rollback past first version: %v", err)
	}
	if _, err := registry.Get("missing", "v1"); !errors.Is(err, crawlrule.ErrVersionNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	// Evaluation owns its output, including nested JSON maps and arrays.
	r := jsonRule()
	r.Fields = []crawlrule.Field{{Name: "data", Pointer: ""}}
	jsonCompiled := compile(t, r)
	raw := response(`{"items":[{"nested":{"title":"A"},"values":[1]}]}`)
	out, _ := jsonCompiled.Evaluate(raw)
	out.Records[0]["data"].(map[string]any)["nested"].(map[string]any)["title"] = "changed"
	out, err = jsonCompiled.Evaluate(raw)
	if err != nil || out.Records[0]["data"].(map[string]any)["nested"].(map[string]any)["title"] != "A" {
		t.Fatalf("result mutation persisted: %+v, %v", out, err)
	}
}

func TestFixtureJSONNumberEqualityAndNestedValues(t *testing.T) {
	r := jsonRule()
	r.Fields = []crawlrule.Field{{Name: "data", Pointer: ""}}
	fixture := crawlrule.Fixture{
		Name: "exact-numbers", Response: response(`{"items":[{"id":9007199254740993,"value":1e3,"tags":[true,null,"ok"]}]}`),
		Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/data", Value: map[string]any{
			"id": json.Number("9007199254740993"), "value": 1000, "tags": []any{true, nil, "ok"},
		}}}},
	}
	report, err := compile(t, r).Verify([]crawlrule.Fixture{fixture})
	if err != nil || !report.Passed {
		t.Fatalf("exact nested JSON expectation failed: %+v, %v", report, err)
	}
	fixture.Expect.Fields[0].Value.(map[string]any)["id"] = json.Number("9007199254740992")
	if _, err := compile(t, r).Verify([]crawlrule.Fixture{fixture}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("large integer mismatch lost to rounding: %v", err)
	}
}

func TestRegistryConcurrentReadersAndActivations(t *testing.T) {
	registry := crawlrule.NewRegistry()
	for i := range 12 {
		r := htmlRule()
		r.Version = fmt.Sprintf("v%d", i)
		if err := registry.Register(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := registry.Activate("catalog", "v0", []crawlrule.Fixture{fixture("A")}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			version := fmt.Sprintf("v%d", i)
			for range 10 {
				if _, err := registry.Activate("catalog", version, []crawlrule.Fixture{fixture("A")}); err != nil {
					t.Error(err)
					return
				}
				c, err := registry.Active("catalog")
				if err != nil {
					t.Error(err)
					return
				}
				copy := c.Rule()
				copy.Fields[0].Selector = ".caller-owned"
				out, err := c.Evaluate(response("<article><h2>A</h2></article>"))
				if err != nil || out.Records[0]["title"] != "A" {
					t.Errorf("concurrent extraction: %+v, %v", out, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
