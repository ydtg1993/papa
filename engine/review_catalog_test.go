package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func reviewCatalogRegister(t *testing.T, reg *crawlrule.Registry, rule crawlrule.Rule, base, body string) {
	t.Helper()
	if err := reg.Register(rule); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	field := rule.Fields[0].Name
	expected := map[string]string{"id": "1", "url": "/detail", "cursor": "value"}[field]
	if _, err := reg.Activate(rule.ID, rule.Version, []crawlrule.Fixture{{Name: "review", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(body)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/" + field, Value: expected}}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewCatalogRejectsPublishedRuleWithMissingHostFields(t *testing.T) {
	const body = `{"rows":[{"id":"1","url":"/detail","cursor":"value"}]}`
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	for _, field := range []string{"id", "url", "cursor"} {
		t.Run(field, func(t *testing.T) {
			reg := ruleTestRegistry(t, srv.URL)
			catalog := &testRuleCatalog{registry: reg}
			catalog.publish("v1", 1)
			cfg := ruleTestConfig(reg, srv.URL)
			cfg.Registry, cfg.Bindings, cfg.Catalog = nil, nil, catalog
			cfg.Links = []RuleLink{{Field: "url", Stage: "detail", Meta: map[string]string{"cursor": "cursor"}}}
			f := ruleTestFetcher(t, cfg)
			old := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			rule, err := reg.Verified("list", "v1")
			if err != nil {
				t.Fatal(err)
			}
			changed := rule.Rule()
			changed.Version = "missing-" + field
			changed.Fields = nil
			for _, candidate := range rule.Rule().Fields {
				if candidate.Name != field {
					changed.Fields = append(changed.Fields, candidate)
				}
			}
			reviewCatalogRegister(t, reg, changed, srv.URL, body)
			catalog.publish("v1", 2)
			catalog.snapshot.Bindings["list"] = crawlrule.Reference{ID: "list", Version: changed.Version}
			before := requests.Load()
			if _, err := f.NewTaskContext(context.Background(), RuleRequest{URL: srv.URL}, nil); err == nil {
				t.Errorf("publication removed host field %q but a permanently unusable task was created", field)
			}
			if requests.Load() != before {
				t.Fatal("task construction contacted the source")
			}
			// A later incompatible publication must not invalidate an old task.
			model := old.toModel()
			children, err := ruleTestRun(t, ruleTestFetcher(t, cfg), (&Engine{}).taskFromRecord(&model))
			if err != nil || len(children) != 1 || children[0].Meta["rule_version"] != "v1" {
				t.Fatalf("old pinned workflow changed: children=%d err=%v", len(children), err)
			}
		})
	}
}

func TestReviewCatalogChecksRestoredTaskAgainstHostBeforeSource(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/detail"}]}`)
	}))
	defer srv.Close()
	reg := ruleTestRegistry(t, srv.URL)
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, srv.URL)
	cfg.Registry, cfg.Bindings, cfg.Catalog = nil, nil, catalog
	seed := ruleTestTask(t, ruleTestFetcher(t, cfg), RuleRequest{URL: srv.URL})
	for _, tc := range []struct {
		name string
		edit func(*RuleFetcherConfig)
	}{
		{"item-key", func(c *RuleFetcherConfig) { c.ItemKey = "absent" }},
		{"link-field", func(c *RuleFetcherConfig) { c.Links = []RuleLink{{Field: "absent", Stage: "detail"}} }},
		{"link-metadata", func(c *RuleFetcherConfig) {
			c.Links = []RuleLink{{Field: "url", Stage: "detail", Meta: map[string]string{"parent": "absent"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := cfg
			tc.edit(&changed)
			saves := 0
			changed.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
			model := seed.toModel()
			before := requests.Load()
			children, err := ruleTestRun(t, ruleTestFetcher(t, changed), (&Engine{}).taskFromRecord(&model))
			if err == nil || core.Retryable(err) || len(children) != 0 || saves != 0 || requests.Load() != before {
				t.Fatalf("invalid pinned contract reached source/business: err=%v requests=%d saves=%d children=%d", err, requests.Load()-before, saves, len(children))
			}
		})
	}
	cfg.Transport = "browser"
	err := ruleTestFetcher(t, cfg).Run(context.Background(), seed, &Engine{}, func(*Task) error { return nil })
	if err == nil || core.Retryable(err) || !strings.Contains(err.Error(), "browser requires HTML rule") {
		t.Fatalf("invalid pinned format reached the browser transport: %v", err)
	}
}

func TestReviewCatalogPermanentVersionFailureAndTemporaryOutage(t *testing.T) {
	reg := ruleTestRegistry(t, "https://example.test")
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, "https://example.test")
	cfg.Registry, cfg.Bindings, cfg.Catalog = nil, nil, catalog
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: "https://example.test/"})
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"corrupt-version", fmt.Errorf("content digest mismatch: %w", crawlrule.ErrInvalidVersion), false},
		{"missing-version", crawlrule.ErrVersionNotFound, false},
		{"unpublished-version", crawlrule.ErrUnverifiedRule, false},
		{"database-outage", errors.New("temporary database outage"), true},
		{"context-timeout", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog.err = tc.err
			err := f.Run(context.Background(), task, &Engine{}, func(*Task) error {
				t.Fatal("invalid/unavailable catalog submitted child")
				return nil
			})
			if !errors.Is(err, tc.err) || core.Retryable(err) != tc.retry {
				t.Fatalf("catalog failure lost identity/classification: retry=%t err=%v", core.Retryable(err), err)
			}
		})
	}
}

func TestReviewCatalogRejectsNonStringLinkContract(t *testing.T) {
	reg := ruleTestRegistry(t, "https://example.test")
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, "https://example.test")
	cfg.Registry, cfg.Bindings, cfg.Catalog = nil, nil, catalog
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	f := ruleTestFetcher(t, cfg)
	compiled, err := reg.Verified("list", "v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		kind       string
		transforms []crawlrule.Transform
	}{
		{"numeric-kind", "number", nil},
		{"numeric-transform", "any", []crawlrule.Transform{{Op: "number"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := compiled.Rule()
			rule.Version = tc.name
			rule.Fields[1].Validation.Kind = tc.kind
			rule.Fields[1].Transforms = tc.transforms
			reviewCatalogRegister(t, reg, rule, "https://example.test/", `{"rows":[{"id":"1","url":42}]}`)
			catalog.snapshot.Bindings["list"] = crawlrule.Reference{ID: "list", Version: rule.Version}
			if _, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/"}, nil); err == nil {
				t.Fatal("numeric link contract created a task that can never produce a string detail URL")
			}
		})
	}
}
