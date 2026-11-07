package crawlrule_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
)

func TestExplicitNegativeFixtures(t *testing.T) {
	c := compile(t, htmlRule())
	positive := fixture("A")
	for _, tc := range []struct {
		name, body, kind, code string
		status                 int
		pass                   bool
	}{
		{"empty-list", "<main>empty</main>", "empty", "", 200, true},
		{"login-as-zero", "<form>login</form>", "empty", "", 200, true},
		{"login-rejection", "<form>login</form>", "reject", "http_status", 401, true},
		{"access-rejection", "denied", "reject", "http_status", 403, true},
		{"error-page", "unavailable", "reject", "http_status", 503, true},
		{"wrong-structure", "<main>different</main>", "reject", "no_records", 200, true},
		{"invalid-field", "<article><h2></h2></article>", "reject", "validation", 200, true},
		{"unexpected-row", "<article><h2>A</h2></article>", "empty", "", 200, false},
		{"status-not-zero", "denied", "empty", "", 403, false},
		{"wrong-error", "<main>empty</main>", "reject", "validation", 200, false},
		{"missing-fields-not-empty", "<article></article>", "empty", "", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			neg := crawlrule.Fixture{Name: tc.name, Response: response(tc.body), Expect: crawlrule.Expectations{Kind: tc.kind, ErrorKind: tc.code}}
			neg.Response.StatusCode = tc.status
			if tc.code == "http_status" {
				neg.Expect.StatusCode = tc.status
			}
			report, err := c.Verify([]crawlrule.Fixture{positive, neg})
			if report.Passed != tc.pass || (err == nil) != tc.pass || report.Positive != 1 || report.Negative != 1 || report.Results[1].Kind != tc.kind {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestNegativeFixturesRequirePositiveCoverageAndStrictContracts(t *testing.T) {
	c := compile(t, htmlRule())
	neg := crawlrule.Fixture{Name: "negative", Response: response("<main>none</main>"), Expect: crawlrule.Expectations{Kind: "empty"}}
	report, err := c.Verify([]crawlrule.Fixture{neg})
	if !errors.Is(err, crawlrule.ErrFixtureFailed) || report.Passed || !report.Results[0].Passed || len(report.Failures) == 0 {
		t.Fatalf("negative-only authorized activation: %+v %v", report, err)
	}
	registry := crawlrule.NewRegistry()
	if err := registry.Register(htmlRule()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Activate("catalog", "v1", []crawlrule.Fixture{neg}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("activated negative-only: %v", err)
	}
	for _, e := range []crawlrule.Expectations{
		{}, {Kind: "unknown"}, {Kind: "empty", RowCount: 1}, {Kind: "empty", Fields: fixture("A").Expect.Fields}, {Kind: "empty", Truncated: true}, {Kind: "empty", NextURL: "https://example.test/next"},
		{Kind: "empty", ErrorKind: "no_records"}, {Kind: "reject", ErrorKind: "execution"}, {Kind: "reject", ErrorKind: "timeout"}, {Kind: "reject", ErrorKind: "http_status", StatusCode: 200}, {Kind: "reject", ErrorKind: "validation", StatusCode: 400},
	} {
		if crawlrule.ValidateExpectations(e) == nil {
			t.Fatalf("invalid expectation accepted: %+v", e)
		}
	}
}

func TestNegativeFixturesNeverAcceptInvalidInputOrExecutionFailure(t *testing.T) {
	rule := jsonRule()
	rule.MaxBodyBytes = 256
	rule.Next = &crawlrule.Next{Pointer: "/next"}
	c := compile(t, rule)
	pos := crawlrule.Fixture{Name: "positive", Response: response(`{"items":[{"title":"A"}]}`), Expect: fixture("A").Expect}
	for _, body := range []string{`{"items":[]`, `{}`, `{"items":null}`, `{"items":[] ,"next":123}`, `{"items":[] ,"next":"https://outside.test/next"}`, `{"items":[],"next":"/page2"}`, `{"items":[{}]}`, strings.Repeat("x", 257)} {
		neg := crawlrule.Fixture{Name: "negative", Response: response(body), Expect: crawlrule.Expectations{Kind: "empty"}}
		report, err := c.Verify([]crawlrule.Fixture{pos, neg})
		if err == nil || report.Results[1].Passed {
			t.Fatalf("malformed/partial data accepted: %s %+v", body, report)
		}
	}
	neg := crawlrule.Fixture{Name: "negative", Response: response(`{"items":[]}`), Expect: crawlrule.Expectations{Kind: "empty"}}
	report, err := c.Verify([]crawlrule.Fixture{pos, neg})
	if err != nil || !report.Passed {
		t.Fatalf("real empty array rejected: %+v %v", report, err)
	}
	for _, kind := range []string{"nil", "url", "status", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			bad := crawlrule.Fixture{Name: "bad", Response: response("denied"), Expect: crawlrule.Expectations{Kind: "reject", ErrorKind: "http_status", StatusCode: 403}}
			bad.Response.StatusCode = 403
			switch kind {
			case "nil":
				bad.Response = nil
			case "url":
				bad.Response.URL = &url.URL{Path: "relative"}
			case "status":
				bad.Response.StatusCode = 0
			case "oversized":
				bad.Response.Body = []byte(strings.Repeat("x", 257))
			}
			report, err := c.Verify([]crawlrule.Fixture{pos, bad})
			if err == nil || report.Results[1].Passed || report.Results[1].ErrorKind != "execution" {
				t.Fatalf("invalid input became rejection: %+v %v", report, err)
			}
		})
	}
}

func TestRenderedNegativeFixturesAndTruncatedRejection(t *testing.T) {
	c := compile(t, htmlRule())
	pos := fixture("A")
	neg := crawlrule.Fixture{Name: "rendered-empty", Response: response("<main>empty</main>"), Rendered: true, Expect: crawlrule.Expectations{Kind: "empty"}}
	neg.Response.StatusCode = 0
	if report, err := c.Verify([]crawlrule.Fixture{pos, neg}); err != nil || !report.Passed {
		t.Fatalf("rendered empty: %+v %v", report, err)
	}
	neg.Expect = crawlrule.Expectations{Kind: "reject", ErrorKind: "http_status", StatusCode: 403}
	if report, err := c.Verify([]crawlrule.Fixture{pos, neg}); err == nil || report.Results[1].Passed {
		t.Fatal("DOM invented HTTP rejection status")
	}
	rule := htmlRule()
	rule.MaxRows = 1
	neg = crawlrule.Fixture{Name: "partial", Response: response("<article><h2></h2></article><article><h2></h2></article>"), Expect: crawlrule.Expectations{Kind: "reject", ErrorKind: "validation"}}
	if report, err := compile(t, rule).Verify([]crawlrule.Fixture{pos, neg}); err == nil || report.Results[1].Passed {
		t.Fatal("truncated rows counted as accepted rejection")
	}
	rule.MaxRows = 10
	rule.MaxIssues = 1
	if report, err := compile(t, rule).Verify([]crawlrule.Fixture{pos, neg}); err == nil || report.Results[1].Passed {
		t.Fatal("truncated validation counted as accepted rejection")
	}
}
