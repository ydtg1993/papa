package crawlrule

import (
	"errors"
	"fmt"

	"github.com/ydtg1993/papa/v3/pkg/discovery"
)

// ValidateExpectations validates host-owned expectations, not model guesses.
// Omitted Kind preserves the original strict positive-fixture contract.
func ValidateExpectations(e Expectations) error {
	switch e.Kind {
	case "", "positive":
		if e.RowCount <= 0 || e.RowCount > MaxRowsLimit || len(e.Fields) == 0 || len(e.Fields) > 1024 || e.ErrorKind != "" || e.StatusCode != 0 {
			return fmt.Errorf("crawlrule: positive fixture requires bounded rows and field expectations only")
		}
	case "empty", "reject":
		if e.RowCount != 0 || len(e.Fields) != 0 || e.NextURL != "" || e.NextCursor != "" || e.Truncated {
			return fmt.Errorf("crawlrule: negative fixture cannot carry positive output expectations")
		}
		if e.Kind == "empty" {
			if e.ErrorKind != "" || e.StatusCode != 0 {
				return fmt.Errorf("crawlrule: empty fixture cannot expect rejection")
			}
			return nil
		}
		if !allowedFixtureError(e.ErrorKind) {
			return fmt.Errorf("crawlrule: unsupported expected rejection")
		}
		if e.ErrorKind == "http_status" {
			if e.StatusCode < 400 || e.StatusCode > 599 {
				return fmt.Errorf("crawlrule: expected HTTP rejection needs exact 400..599 status")
			}
		} else if e.StatusCode != 0 {
			return fmt.Errorf("crawlrule: status only applies to HTTP rejection")
		}
	default:
		return fmt.Errorf("crawlrule: unknown fixture kind")
	}
	return nil
}

func allowedFixtureError(kind string) bool {
	return kind == "http_status" || kind == "no_records" || kind == "validation"
}
func fixtureErrorKind(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrHTTPStatus):
		return "http_status"
	case errors.Is(err, ErrNoRecords):
		return "no_records"
	case errors.Is(err, ErrValidation):
		return "validation"
	default:
		return "execution"
	}
}

func (c *Compiled) validateFixtureInput(f Fixture) error {
	if c == nil || c.validator == nil || f.Response == nil || f.Response.URL == nil {
		return fmt.Errorf("crawlrule: complete fixture input required")
	}
	if _, err := discovery.CanonicalURL(f.Response.URL.String(), ""); err != nil {
		return fmt.Errorf("crawlrule: invalid fixture URL")
	}
	if len(f.Response.Body) > c.rule.MaxBodyBytes {
		return ErrBodyTooLarge
	}
	if (!f.Rendered && (f.Response.StatusCode < 100 || f.Response.StatusCode > 599)) || (f.Rendered && f.Response.StatusCode != 0) {
		return fmt.Errorf("crawlrule: invalid fixture status")
	}
	return nil
}

// SummarizeFixtures applies the whole-set positive-coverage gate. A passing
// negative result alone is useful diagnostically but cannot authorize activation.
func SummarizeFixtures(results []FixtureResult) FixtureReport {
	report := FixtureReport{Passed: len(results) > 0, Results: results}
	for _, r := range results {
		switch r.Kind {
		case "", "positive":
			report.Positive++
		case "empty", "reject":
			report.Negative++
		default:
			report.Passed = false
		}
		report.Passed = report.Passed && r.Passed
	}
	if report.Positive == 0 {
		report.Passed = false
		report.Failures = []string{"at least one positive fixture required"}
	}
	return report
}
