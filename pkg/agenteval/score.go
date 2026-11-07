// Package agenteval measures fixed host-authored evaluations. It has no model,
// network, database mutation or publication capabilities.
package agenteval

import (
	"errors"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
)

// Count retains the denominator, including failed or unattempted samples.
type Count struct {
	Matched int `json:"matched"`
	Total   int `json:"total"`
}

func (c Count) Fraction() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Matched) / float64(c.Total)
}

type Scores struct {
	Samples           Count `json:"samples"`
	Fields            Count `json:"required_field_assertions"`
	Negative          Count `json:"negative"`
	ExecutionFailures int   `json:"execution_failures"`
}

// Score measures held-out fixtures after generation has ended. Fields must be
// the host's fixed required-field assertions. A nil candidate scores zero with
// unchanged denominators. Partial field matches are diagnostic, never approval.
func Score(c *crawlrule.Compiled, fixtures []crawlrule.Fixture) (Scores, error) {
	var score Scores
	positive := 0
	names := map[string]bool{}
	if len(fixtures) == 0 || len(fixtures) > 64 {
		return score, errors.New("agenteval: require 1..64 fixed fixtures")
	}
	for _, f := range fixtures {
		if f.Name == "" || names[f.Name] || crawlrule.ValidateExpectations(f.Expect) != nil {
			return Scores{}, errors.New("agenteval: invalid fixed expectations")
		}
		names[f.Name] = true
		score.Samples.Total++
		if f.Expect.Kind == "empty" || f.Expect.Kind == "reject" {
			score.Negative.Total++
		} else {
			positive++
			score.Fields.Total += len(f.Expect.Fields)
		}
	}
	if positive == 0 {
		return Scores{}, errors.New("agenteval: positive coverage required")
	}
	if c == nil {
		return score, nil
	}
	report, _ := c.Verify(fixtures)
	for i, r := range report.Results {
		f := fixtures[i]
		if r.Passed {
			score.Samples.Matched++
		}
		if r.ErrorKind == "execution" {
			score.ExecutionFailures++
		}
		if f.Expect.Kind == "empty" || f.Expect.Kind == "reject" {
			if r.Passed {
				score.Negative.Matched++
			}
			continue
		}
		// Never parse an input that Verify rejected as malformed or over budget.
		if r.ErrorKind == "execution" {
			continue
		}
		var out crawlrule.Result
		if f.Rendered {
			out, _ = c.EvaluateRendered(f.Response.URL.String(), f.Response.Body)
		} else {
			out, _ = c.Evaluate(f.Response)
		}
		score.Fields.Matched += crawlrule.CountMatchingFields(out.Records, f.Expect.Fields)
	}
	return score, nil
}
