package rulestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

const (
	MaxComparisonChanges = 128
	MaxShadowResultBytes = 16 << 20
	MaxShadowCells       = 100000
	MaxShadowValueNodes  = 262144
	MaxShadowValueDepth  = 64
)

var (
	ErrComparisonInvalid = errors.New("rulestore: compiled rules required")
	ErrComparisonBudget  = errors.New("rulestore: comparison exceeds budget")
	ErrShadowInput       = errors.New("rulestore: invalid offline shadow input")
)

// RuleChange identifies a normalized JSON path without repeating rule values.
// Paths use fixed schema property names and zero-based array indexes.
type RuleChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // added, removed or changed
}

type RuleComparison struct {
	CurrentSHA256   string       `json:"current_sha256"`
	CandidateSHA256 string       `json:"candidate_sha256"`
	Changes         []RuleChange `json:"changes"`
	Truncated       bool         `json:"truncated"`
}

// CompareRules compares immutable compiled snapshots, including their resolved
// defaults, selectors, validation, transforms, pagination and budgets. It is
// structural: it does not promise equivalence of different CSS expressions.
func CompareRules(current, candidate *crawlrule.Compiled) (RuleComparison, error) {
	var report RuleComparison
	a, err := normalizedRule(current)
	if err != nil {
		return report, err
	}
	b, err := normalizedRule(candidate)
	if err != nil {
		return report, err
	}
	report.CurrentSHA256, report.CandidateSHA256 = bodySHA256(a), bodySHA256(b)
	var left, right any
	if err := htmlfetch.DecodeJSON(a, &left); err != nil {
		return report, ErrComparisonInvalid
	}
	if err := htmlfetch.DecodeJSON(b, &right); err != nil {
		return report, ErrComparisonInvalid
	}
	var walk func(string, any, any)
	add := func(path, kind string) {
		if len(report.Changes) == MaxComparisonChanges {
			report.Truncated = true
			return
		}
		report.Changes = append(report.Changes, RuleChange{Path: path, Kind: kind})
	}
	walk = func(path string, a, b any) {
		if report.Truncated {
			return
		}
		if x, ok := a.(map[string]any); ok {
			if y, ok := b.(map[string]any); ok {
				keys := make([]string, 0, len(x)+len(y))
				for key := range x {
					keys = append(keys, key)
				}
				for key := range y {
					if _, exists := x[key]; !exists {
						keys = append(keys, key)
					}
				}
				sort.Strings(keys)
				for _, key := range keys {
					av, ae := x[key]
					bv, be := y[key]
					switch {
					case !ae:
						add(path+"."+key, "added")
					case !be:
						add(path+"."+key, "removed")
					default:
						walk(path+"."+key, av, bv)
					}
					if report.Truncated {
						return
					}
				}
				return
			}
		}
		if x, ok := a.([]any); ok {
			if y, ok := b.([]any); ok {
				for i := 0; i < max(len(x), len(y)); i++ {
					child := path + "[" + strconv.Itoa(i) + "]"
					switch {
					case i >= len(x):
						add(child, "added")
					case i >= len(y):
						add(child, "removed")
					default:
						walk(child, x[i], y[i])
					}
					if report.Truncated {
						return
					}
				}
				return
			}
		}
		if !reflect.DeepEqual(a, b) {
			add(path, "changed")
		}
	}
	walk("$", left, right)
	return report, nil
}

func normalizedRule(c *crawlrule.Compiled) ([]byte, error) {
	if c == nil {
		return nil, ErrComparisonInvalid
	}
	r := c.Rule()
	if r.SchemaVersion != crawlrule.SchemaVersion || len(r.Fields) == 0 {
		return nil, ErrComparisonInvalid
	}
	// Compile also accepts Go values directly; enforce the JSON size ceiling
	// here, before allocating a potentially unbounded serialization.
	size := len(r.ID) + len(r.Version) + len(r.Format) + len(r.Rows)
	for _, f := range r.Fields {
		size += len(f.Name) + len(f.Selector) + len(f.Attribute) + len(f.Pointer) + len(f.Validation.Kind) + len(f.Validation.Pattern)
		for _, tr := range f.Transforms {
			size += len(tr.Op) + len(tr.Layout)
		}
	}
	if r.Next != nil {
		size += len(r.Next.Mode) + len(r.Next.Selector) + len(r.Next.Attribute) + len(r.Next.Pointer)
	}
	if size > crawlrule.MaxRuleBytes {
		return nil, ErrComparisonBudget
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, ErrComparisonInvalid
	}
	if len(data) > crawlrule.MaxRuleBytes {
		return nil, ErrComparisonBudget
	}
	return data, nil
}

// ShadowInput is a single already-fetched response or rendered DOM. No client,
// storage hook or task submitter is accepted. Callers must not mutate it during
// Shadow. Rendered DOM uses status zero and is evaluated only by HTML rules.
type ShadowInput struct {
	Response *htmlfetch.Response
	Rendered bool
}

type ShadowOutcome struct {
	Success       bool   `json:"success"`
	ErrorKind     string `json:"error_kind,omitempty"`
	Rows          int    `json:"rows"`
	InvalidRows   int    `json:"invalid_rows"`
	MissingFields int    `json:"missing_fields"`
	ResultSHA256  string `json:"result_sha256,omitempty"`
	Truncated     bool   `json:"truncated"`
}

type ShadowReport struct {
	Current           ShadowOutcome `json:"current"`
	Candidate         ShadowOutcome `json:"candidate"`
	ResultChanged     bool          `json:"result_changed"`
	NextURLChanged    bool          `json:"next_url_changed"`
	NextCursorChanged bool          `json:"next_cursor_changed"`
}

// Shadow evaluates both rules sequentially on one private input snapshot. Rule
// failures are reported by fixed category and are not top-level errors. Invalid
// inputs and comparison budgets return errors. It does not verify expectations
// or authorize publication, and never requests a URL, saves, or submits work.
func Shadow(current, candidate *crawlrule.Compiled, input ShadowInput) (ShadowReport, error) {
	var report ShadowReport
	for _, c := range []*crawlrule.Compiled{current, candidate} {
		if _, err := normalizedRule(c); err != nil {
			return report, err
		}
		r := c.Rule()
		if r.MaxRows > MaxShadowCells/len(r.Fields) {
			return report, ErrComparisonBudget
		}
	}
	res := input.Response
	if res == nil || res.URL == nil || !validSampleStatus(res.StatusCode, input.Rendered) {
		return report, ErrShadowInput
	}
	if len(res.Body) > MaxSampleBodyBytes || len(res.URL.String()) > MaxSampleURLBytes {
		return report, ErrComparisonBudget
	}
	u := *res.URL
	res = &htmlfetch.Response{URL: &u, StatusCode: res.StatusCode, Body: bytes.Clone(res.Body)}
	a, anext, acursor, aerr := shadowOne(current, res, input.Rendered)
	b, bnext, bcursor, berr := shadowOne(candidate, res, input.Rendered)
	report.Current, report.Candidate = a, b
	report.ResultChanged = a.ResultSHA256 != b.ResultSHA256 || a.Success != b.Success || a.ErrorKind != b.ErrorKind || a.Truncated != b.Truncated
	report.NextURLChanged, report.NextCursorChanged = anext != bnext, acursor != bcursor
	if aerr != nil || berr != nil {
		return report, ErrComparisonBudget
	}
	return report, nil
}

func shadowOne(c *crawlrule.Compiled, response *htmlfetch.Response, rendered bool) (ShadowOutcome, string, string, error) {
	var result crawlrule.Result
	var err error
	if rendered {
		result, err = c.EvaluateRendered(response.URL.String(), response.Body)
	} else {
		result, err = c.Evaluate(response)
	}
	out := ShadowOutcome{Success: err == nil, ErrorKind: shadowErrorKind(err, response.StatusCode, rendered), Rows: len(result.Records), InvalidRows: result.Validation.InvalidRows, Truncated: result.Truncated}
	fields := c.Rule().Fields
	for _, record := range result.Records {
		for _, field := range fields {
			if _, ok := record[field.Name]; !ok {
				out.MissingFields++
			}
		}
	}
	digest, hashErr := shadowResultSHA(result.Records)
	if hashErr != nil {
		out.Success = false
		out.ErrorKind = "result_budget"
	} else {
		out.ResultSHA256 = digest
	}
	return out, result.NextURL, result.NextCursor, hashErr
}

func shadowErrorKind(err error, status int, rendered bool) string {
	if err == nil {
		return ""
	}
	if !rendered && (status < 200 || status >= 300) {
		return "response_status"
	}
	for _, item := range []struct {
		err  error
		kind string
	}{
		{crawlrule.ErrBodyTooLarge, "body_budget"}, {crawlrule.ErrNoRecords, "no_records"},
		{crawlrule.ErrValidation, "validation"}, {crawlrule.ErrTransform, "transform"},
		{crawlrule.ErrNextURL, "next_url"}, {crawlrule.ErrNextCursor, "next_cursor"},
	} {
		if errors.Is(err, item.err) {
			return item.kind
		}
	}
	return "evaluation"
}

func shadowResultSHA(records []map[string]any) (string, error) {
	// Bound nesting, traversal and serialized size before json.Marshal allocates
	// output. Repeated JSON subtrees are counted for each extracted occurrence.
	remaining, nodes := MaxShadowResultBytes, MaxShadowValueNodes
	var visit func(any, int) bool
	charge := func(n int) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	visit = func(v any, depth int) bool {
		nodes--
		if nodes < 0 || depth > MaxShadowValueDepth || !charge(2) {
			return false
		}
		switch x := v.(type) {
		case nil:
			return charge(4)
		case bool:
			return charge(5)
		case json.Number:
			return charge(len(x))
		case string:
			return len(x) <= remaining/6 && charge(len(x)*6)
		case []any:
			for _, child := range x {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		case map[string]any:
			for key, child := range x {
				if len(key) > remaining/6 || !charge(len(key)*6+3) || !visit(child, depth+1) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	for _, record := range records {
		if !visit(record, 0) {
			return "", ErrComparisonBudget
		}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("%w: result encoding", ErrComparisonBudget)
	}
	if len(data) > MaxShadowResultBytes {
		return "", ErrComparisonBudget
	}
	return bodySHA256(data), nil
}
