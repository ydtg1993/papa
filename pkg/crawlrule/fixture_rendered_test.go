package crawlrule_test

import (
	"errors"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestVerifyRenderedFixturePreservesUnknownHTTPStatus(t *testing.T) {
	c := compile(t, htmlRule())
	res := response("<article><h2>rendered title</h2></article>")
	res.StatusCode = 0
	fixture := crawlrule.Fixture{Name: "DOM", Response: res, Rendered: true, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: "rendered title"}}}}
	report, err := c.Verify([]crawlrule.Fixture{fixture})
	if err != nil || !report.Passed || res.StatusCode != 0 {
		t.Fatalf("rendered verify: %+v %v", report, err)
	}
	fixture.Rendered = false
	if _, err := c.Verify([]crawlrule.Fixture{fixture}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("HTTP status zero: %v", err)
	}
	fixture.Rendered = true
	res.StatusCode = 200
	if _, err := c.Verify([]crawlrule.Fixture{fixture}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("fabricated status: %v", err)
	}
	res.StatusCode = 0
	fixture.Response = nil
	if _, err := c.Verify([]crawlrule.Fixture{fixture}); !errors.Is(err, crawlrule.ErrFixtureFailed) {
		t.Fatalf("nil rendered response: %v", err)
	}
}
