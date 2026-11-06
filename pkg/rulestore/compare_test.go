package rulestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func compareTestRule(t *testing.T, version string, edit func(*crawlrule.Rule)) *crawlrule.Compiled {
	t.Helper()
	r := crawlrule.Rule{SchemaVersion: 1, ID: "catalog", Version: version, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "h2", Validation: crawlrule.Validation{Required: true}}}}
	if edit != nil {
		edit(&r)
	}
	c, err := crawlrule.Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func compareTestResponse(body string) *htmlfetch.Response {
	u, _ := url.Parse("https://example.test/private?token=secret-token")
	return &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(body)}
}

func TestCompareRulesNormalizedPathsAndChangeLimit(t *testing.T) {
	a := compareTestRule(t, "v1", nil)
	b := compareTestRule(t, "v1", func(r *crawlrule.Rule) {
		r.MaxRows = crawlrule.DefaultMaxRows
		r.MaxBodyBytes = crawlrule.DefaultMaxBodyBytes
	})
	report, err := CompareRules(a, b)
	if err != nil || len(report.Changes) != 0 || report.CurrentSHA256 != report.CandidateSHA256 {
		t.Fatalf("normalized defaults: %+v %v", report, err)
	}
	b = compareTestRule(t, "v2", func(r *crawlrule.Rule) {
		r.Fields[0].Selector = "h3.secret-selector"
		r.Fields[0].Transforms = []crawlrule.Transform{{Op: "trim"}}
		r.MaxRows = 20
		r.Next = &crawlrule.Next{Selector: "a.next"}
	})
	report, err = CompareRules(a, b)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, change := range report.Changes {
		paths[change.Path] = true
	}
	for _, path := range []string{"$.fields[0].selector", "$.fields[0].transforms", "$.max_rows", "$.next", "$.version"} {
		if !paths[path] {
			t.Fatalf("missing path %s: %+v", path, report)
		}
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "secret-selector") {
		t.Fatalf("rule value leaked: %s", raw)
	}
	a = compareTestRule(t, "v1", func(r *crawlrule.Rule) {
		r.Fields = nil
		for i := 0; i < 200; i++ {
			r.Fields = append(r.Fields, crawlrule.Field{Name: fmt.Sprintf("f%d", i), Selector: "h2"})
		}
	})
	b = compareTestRule(t, "v2", func(r *crawlrule.Rule) {
		r.Fields = nil
		for i := 0; i < 200; i++ {
			r.Fields = append(r.Fields, crawlrule.Field{Name: fmt.Sprintf("f%d", i), Selector: "h3"})
		}
	})
	report, err = CompareRules(a, b)
	if err != nil || len(report.Changes) != MaxComparisonChanges || !report.Truncated {
		t.Fatalf("diff budget: %+v %v", report, err)
	}
}

func TestShadowSameOfflineResponseHasNoRequestsOrSensitiveReportValues(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/private?token=secret-token")
	response := &htmlfetch.Response{URL: u, StatusCode: 200, Header: http.Header{"Cookie": []string{"secret-cookie"}}, Body: []byte(`<article><h2>secret-title</h2><h3>changed-secret-title</h3></article><a class="next" href="?page=2&token=secret-next">next</a>`)}
	a := compareTestRule(t, "v1", nil)
	b := compareTestRule(t, "v2", func(r *crawlrule.Rule) { r.Fields[0].Selector = "h3"; r.Next = &crawlrule.Next{Selector: "a.next"} })
	report, err := Shadow(a, b, ShadowInput{Response: response})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Current.Success || !report.Candidate.Success || !report.ResultChanged || !report.NextURLChanged || report.NextCursorChanged || report.Current.Rows != 1 {
		t.Fatalf("shadow: %+v", report)
	}
	raw, _ := json.Marshal(report)
	for _, secret := range []string{"secret", "http://", "private", "Cookie", "h2", "h3"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("report leaked %s: %s", secret, raw)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("offline evaluation issued request")
	}
	if response.Header.Get("Cookie") != "secret-cookie" || !strings.Contains(string(response.Body), "secret-title") {
		t.Fatal("input mutated")
	}
	copyReport, err := Shadow(a, a, ShadowInput{Response: response})
	if err != nil || copyReport.ResultChanged || copyReport.NextURLChanged || copyReport.Current.ResultSHA256 != copyReport.Candidate.ResultSHA256 {
		t.Fatalf("same response: %+v %v", copyReport, err)
	}
}

func TestShadowReportsMissingFieldsAndSafeErrorCategories(t *testing.T) {
	a := compareTestRule(t, "v1", nil)
	b := compareTestRule(t, "v2", func(r *crawlrule.Rule) { r.Fields[0].Selector = "h3" })
	report, err := Shadow(a, b, ShadowInput{Response: compareTestResponse(`<article><h2>secret</h2></article>`)})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Current.Success || report.Candidate.Success || report.Candidate.ErrorKind != "validation" || report.Candidate.MissingFields != 1 || report.Candidate.InvalidRows != 1 {
		t.Fatalf("validation: %+v", report)
	}
	res := compareTestResponse("<article><h2>secret</h2></article>")
	res.StatusCode = 403
	report, err = Shadow(a, b, ShadowInput{Response: res})
	if err != nil || report.Current.ErrorKind != "response_status" || report.Candidate.ErrorKind != "response_status" {
		t.Fatalf("status: %+v %v", report, err)
	}
	res.StatusCode = 200
	b = compareTestRule(t, "v2", func(r *crawlrule.Rule) { r.Fields[0].Transforms = []crawlrule.Transform{{Op: "number"}} })
	report, err = Shadow(a, b, ShadowInput{Response: res})
	if err != nil || report.Candidate.ErrorKind != "transform" {
		t.Fatalf("transform: %+v %v", report, err)
	}
	b = compareTestRule(t, "v2", func(r *crawlrule.Rule) { r.MaxBodyBytes = 1 })
	report, err = Shadow(a, b, ShadowInput{Response: res})
	if err != nil || report.Candidate.ErrorKind != "body_budget" {
		t.Fatalf("body: %+v %v", report, err)
	}
}

func TestShadowRenderedAndCursorSemantics(t *testing.T) {
	a := compareTestRule(t, "v1", nil)
	res := compareTestResponse("<article><h2>body</h2></article>")
	res.StatusCode = 0
	report, err := Shadow(a, a, ShadowInput{Response: res, Rendered: true})
	if err != nil || !report.Current.Success {
		t.Fatalf("rendered: %+v %v", report, err)
	}
	if _, err := Shadow(a, a, ShadowInput{Response: res}); !errors.Is(err, ErrShadowInput) {
		t.Fatalf("HTTP status zero: %v", err)
	}
	res.StatusCode = 200
	if _, err := Shadow(a, a, ShadowInput{Response: res, Rendered: true}); !errors.Is(err, ErrShadowInput) {
		t.Fatalf("fabricated rendered status: %v", err)
	}
	jsonRule := func(r *crawlrule.Rule) {
		r.Format = "json"
		r.Rows = "/items"
		r.Fields = []crawlrule.Field{{Name: "id", Pointer: "/id"}}
		r.Next = &crawlrule.Next{Mode: "cursor", Pointer: "/next"}
	}
	a = compareTestRule(t, "v1", jsonRule)
	b := compareTestRule(t, "v2", func(r *crawlrule.Rule) { jsonRule(r); r.Next.Pointer = "/other" })
	res = compareTestResponse(`{"items":[{"id":9007199254740993123}],"next":"secret-cursor","other":"different-secret-cursor"}`)
	report, err = Shadow(a, b, ShadowInput{Response: res})
	if err != nil || !report.NextCursorChanged || report.NextURLChanged || report.ResultChanged {
		t.Fatalf("cursor: %+v %v", report, err)
	}
	res.StatusCode = 0
	report, err = Shadow(a, a, ShadowInput{Response: res, Rendered: true})
	if err != nil || report.Current.ErrorKind != "evaluation" || report.Current.Success {
		t.Fatalf("JSON DOM: %+v %v", report, err)
	}
}

func TestComparisonRejectsInvalidRulesAndBudgets(t *testing.T) {
	a := compareTestRule(t, "v1", nil)
	for _, invalid := range []*crawlrule.Compiled{nil, {}} {
		if _, err := CompareRules(a, invalid); !errors.Is(err, ErrComparisonInvalid) {
			t.Fatalf("invalid comparison: %v", err)
		}
		if _, err := Shadow(a, invalid, ShadowInput{}); !errors.Is(err, ErrComparisonInvalid) {
			t.Fatalf("invalid shadow: %v", err)
		}
	}
	if _, err := Shadow(a, a, ShadowInput{}); !errors.Is(err, ErrShadowInput) {
		t.Fatal(err)
	}
	res := compareTestResponse("")
	res.Body = make([]byte, MaxSampleBodyBytes+1)
	if _, err := Shadow(a, a, ShadowInput{Response: res}); !errors.Is(err, ErrComparisonBudget) {
		t.Fatal(err)
	}
	b := compareTestRule(t, "v2", func(r *crawlrule.Rule) {
		r.MaxRows = crawlrule.MaxRowsLimit
		r.Fields = append(r.Fields, crawlrule.Field{Name: "second", Selector: "h2"})
	})
	if _, err := Shadow(a, b, ShadowInput{Response: compareTestResponse("<article></article>")}); !errors.Is(err, ErrComparisonBudget) {
		t.Fatalf("cell budget: %v", err)
	}
	if _, err := shadowResultSHA([]map[string]any{{"value": strings.Repeat("x", MaxShadowResultBytes/6+1)}}); !errors.Is(err, ErrComparisonBudget) {
		t.Fatalf("result budget: %v", err)
	}
	var deep any = "value"
	for i := 0; i <= MaxShadowValueDepth; i++ {
		deep = []any{deep}
	}
	if _, err := shadowResultSHA([]map[string]any{{"deep": deep}}); !errors.Is(err, ErrComparisonBudget) {
		t.Fatalf("depth budget: %v", err)
	}
}
