package crawlrule

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// Fixture is an explicit, already fetched and preferably redacted sample.
// Callers must not modify Response or expectations during Verify/Activate.
// Samples are not retained by the registry and are never fetched automatically.
type Fixture struct {
	Name     string
	Response *htmlfetch.Response
	// Rendered selects HTML DOM verification. Its Response must have status 0,
	// because a rendered snapshot does not establish an HTTP success status.
	Rendered bool
	Expect   Expectations
}

// Expectations defaults to a positive exact RowCount and field assertions.
// Explicit empty/reject kinds are host-defined negative fixtures; they cannot
// replace positive coverage. NextURL/NextCursor are exact for positive fixtures.
type Expectations struct {
	Kind       string `json:",omitempty"` // empty defaults to positive; otherwise positive, empty or reject
	ErrorKind  string `json:",omitempty"` // reject: http_status, no_records or validation
	StatusCode int    `json:",omitempty"` // reject/http_status: exact 400..599 status
	RowCount   int
	Fields     []FieldExpectation
	NextURL    string
	NextCursor string
	Truncated  bool
}

// FieldExpectation uses a zero-based row index and a JSON Pointer into the
// output record. Value must be JSON-serializable; use json.Number for large IDs.
// A missing value never equals null. Numeric comparisons preserve precision.
type FieldExpectation struct {
	Row     int
	Pointer string
	Value   any
}

type FixtureResult struct {
	Kind        string `json:",omitempty"`
	ErrorKind   string `json:",omitempty"`
	StatusCode  int    `json:",omitempty"`
	Name        string
	Passed      bool
	Rows        int
	InvalidRows int
	Truncated   bool
	Failures    []string // describes mismatches without response or field values
}

type FixtureReport struct {
	Passed   bool
	Results  []FixtureResult
	Positive int      `json:",omitempty"`
	Negative int      `json:",omitempty"`
	Failures []string `json:",omitempty"`
}

// Verify evaluates every fixture. At least one positive fixture and all fixture
// expectations must pass. Only explicitly allowed rejection kinds can succeed
// as negative fixtures; input/resource/execution errors never qualify.
func (c *Compiled) Verify(fixtures []Fixture) (FixtureReport, error) {
	if len(fixtures) == 0 {
		return FixtureReport{}, ErrNoFixtures
	}
	report := FixtureReport{Passed: true}
	names := map[string]bool{}
	for _, fixture := range fixtures {
		result := FixtureResult{Name: fixture.Name, Kind: fixture.Expect.Kind}
		fail := func(message string) { result.Failures = append(result.Failures, message) }
		if strings.TrimSpace(fixture.Name) == "" || names[fixture.Name] {
			fail("fixture name must be nonblank and unique")
		}
		names[fixture.Name] = true
		if err := ValidateExpectations(fixture.Expect); err != nil {
			fail(err.Error())
		}
		var out Result
		var err error
		if err = c.validateFixtureInput(fixture); err != nil {
			// Invalid metadata or resource limits are never expected rejection.
		} else if fixture.Rendered {
			out, err = c.EvaluateRendered(fixture.Response.URL.String(), fixture.Response.Body)
		} else {
			out, err = c.Evaluate(fixture.Response)
		}
		result.Rows, result.InvalidRows, result.Truncated = len(out.Records), out.Validation.InvalidRows, out.Truncated
		result.ErrorKind = fixtureErrorKind(err)
		if result.ErrorKind == "http_status" {
			result.StatusCode = fixture.Response.StatusCode
		}
		if fixture.Expect.Kind == "empty" {
			if err != ErrNoRecords || len(out.Records) != 0 || out.Validation.InvalidRows != 0 || out.Truncated || out.NextURL != "" || out.NextCursor != "" {
				fail("expected complete empty result")
			}
		} else if fixture.Expect.Kind == "reject" {
			if err == nil || result.ErrorKind != fixture.Expect.ErrorKind || !allowedFixtureError(result.ErrorKind) || out.Truncated || out.Validation.Truncated || (result.ErrorKind == "http_status" && result.StatusCode != fixture.Expect.StatusCode) {
				fail("expected rejection mismatch")
			}
		} else if err != nil {
			fail("evaluation failed: " + err.Error())
		} else {
			if len(out.Records) != fixture.Expect.RowCount {
				fail("row count mismatch")
			}
			if out.NextURL != fixture.Expect.NextURL {
				fail("next URL mismatch")
			}
			if out.NextCursor != fixture.Expect.NextCursor {
				fail("next cursor mismatch")
			}
			if out.Truncated != fixture.Expect.Truncated {
				fail("row truncation mismatch")
			}
			for i, field := range fixture.Expect.Fields {
				label := fmt.Sprintf("field expectation %d", i)
				tokens, err := parsePointer(field.Pointer)
				if err != nil || field.Row < 0 || field.Row >= len(out.Records) {
					fail(label + ": invalid pointer or row index")
					continue
				}
				value, exists := lookup(out.Records[field.Row], tokens)
				if !exists {
					fail(label + ": value is missing")
					continue
				}
				raw, err := json.Marshal(field.Value)
				if err != nil {
					fail(label + ": expected value is not JSON-serializable")
					continue
				}
				var expected any
				if err := htmlfetch.DecodeJSON(raw, &expected); err != nil || !equalJSON(value, expected) {
					fail(label + ": value mismatch")
				}
			}
		}
		result.Passed = len(result.Failures) == 0
		report.Passed = report.Passed && result.Passed
		report.Results = append(report.Results, result)
	}
	report = SummarizeFixtures(report.Results)
	if !report.Passed {
		return report, ErrFixtureFailed
	}
	return report, nil
}

func equalJSON(actual, expected any) bool {
	switch a := actual.(type) {
	case nil:
		return expected == nil
	case string:
		b, ok := expected.(string)
		return ok && a == b
	case bool:
		b, ok := expected.(bool)
		return ok && a == b
	case json.Number:
		b, ok := expected.(json.Number)
		if !ok {
			return false
		}
		if a == b {
			return true
		}
		x, y := boundedNumber(a), boundedNumber(b)
		return x != nil && y != nil && x.Cmp(y) == 0
	case []any:
		b, ok := expected.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !equalJSON(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		b, ok := expected.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, exists := b[key]
			if !exists || !equalJSON(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func boundedNumber(value json.Number) *big.Rat {
	s := value.String()
	if len(s) > 512 {
		return nil
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(s[i+1:])
		if err != nil || exponent < -4096 || exponent > 4096 {
			return nil
		}
	}
	r, _ := new(big.Rat).SetString(s)
	return r
}
