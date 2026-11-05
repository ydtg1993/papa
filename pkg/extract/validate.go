// Package extract validates parsed crawl records independently of task status.
package extract

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type FieldRule struct {
	Pointer   string // JSON Pointer, e.g. /title, /author/name, /tags/0
	Kind      string // any, string, number, integer, bool, object, array, http-url
	Required  bool
	AllowNull bool
	MinLength int    // Unicode codepoints for strings; elements for arrays
	Pattern   string // Go regular expression, string kinds only
	Unique    bool   // batch uniqueness; missing and allowed-null values are skipped
}
type compiledRule struct {
	FieldRule
	tokens  []string
	pattern *regexp.Regexp
}
type Validator struct{ rules []compiledRule }
type Issue struct {
	Row     int
	Pointer string
	Code    string
}
type Report struct {
	Rows        int
	InvalidRows int
	TotalIssues int
	Issues      []Issue
	Truncated   bool
}

func NewValidator(rules []FieldRule) (*Validator, error) {
	if len(rules) == 0 {
		return nil, fmt.Errorf("at least one field rule required")
	}
	v := &Validator{}
	seen := map[string]bool{}
	for _, rule := range rules {
		if seen[rule.Pointer] {
			return nil, fmt.Errorf("duplicate field pointer")
		}
		seen[rule.Pointer] = true
		if rule.MinLength < 0 {
			return nil, fmt.Errorf("negative minimum length")
		}
		switch rule.Kind {
		case "any", "string", "number", "integer", "bool", "object", "array", "http-url":
		default:
			return nil, fmt.Errorf("unsupported field kind %q", rule.Kind)
		}
		if rule.MinLength > 0 && rule.Kind != "string" && rule.Kind != "http-url" && rule.Kind != "array" {
			return nil, fmt.Errorf("length constraint requires string or array")
		}
		c := compiledRule{FieldRule: rule}
		if rule.Pointer != "" {
			if !strings.HasPrefix(rule.Pointer, "/") {
				return nil, fmt.Errorf("field must use JSON Pointer")
			}
			for _, part := range strings.Split(rule.Pointer[1:], "/") {
				for i := 0; i < len(part); i++ {
					if part[i] == '~' {
						if i+1 >= len(part) || (part[i+1] != '0' && part[i+1] != '1') {
							return nil, fmt.Errorf("invalid JSON Pointer escape")
						}
						i++
					}
				}
				c.tokens = append(c.tokens, strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~"))
			}
		}
		if rule.Pattern != "" {
			if rule.Kind != "string" && rule.Kind != "http-url" {
				return nil, fmt.Errorf("pattern requires string kind")
			}
			var err error
			c.pattern, err = regexp.Compile(rule.Pattern)
			if err != nil {
				return nil, err
			}
		}
		v.rules = append(v.rules, c)
	}
	return v, nil
}

func lookup(value any, tokens []string) (any, bool) {
	for _, key := range tokens {
		switch x := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = x[key]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != key {
				return nil, false
			}
			value = x[i]
		default:
			return nil, false
		}
	}
	return value, true
}

func number(value any) (valid, integer bool) {
	switch x := value.(type) {
	case json.Number:
		// Bound exponent expansion before constructing an exact rational.
		if len(x.String()) > 512 {
			return false, false
		}
		if i := strings.IndexAny(x.String(), "eE"); i >= 0 {
			exponent, err := strconv.Atoi(x.String()[i+1:])
			if err != nil || exponent < -4096 || exponent > 4096 {
				return false, false
			}
		}
		if !json.Valid([]byte(x.String())) {
			return false, false
		}
		r, ok := new(big.Rat).SetString(x.String())
		return ok, ok && r.IsInt()
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0), !math.IsNaN(x) && !math.IsInf(x, 0) && math.Trunc(x) == x
	default:
		return false, false
	}
}

func kindMatches(value any, kind string) bool {
	switch kind {
	case "any":
		return true
	case "string", "http-url":
		_, ok := value.(string)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "bool":
		_, ok := value.(bool)
		return ok
	case "number":
		ok, _ := number(value)
		return ok
	case "integer":
		_, ok := number(value)
		return ok
	}
	return false
}

// Validate consumes standard encoding/json values (prefer htmlfetch.DecodeJSON
// for exact numbers). It never modifies records or includes field values in issues.
// maxIssues caps retained details, not validation work or the total issue count.
func (v *Validator) Validate(rows []map[string]any, maxIssues int) Report {
	if maxIssues <= 0 {
		maxIssues = 100
	}
	report := Report{Rows: len(rows)}
	unique := make([]map[string]bool, len(v.rules))
	for i := range unique {
		unique[i] = map[string]bool{}
	}
	for row, record := range rows {
		invalid := false
		add := func(pointer, code string) {
			invalid = true
			report.TotalIssues++
			if len(report.Issues) < maxIssues {
				report.Issues = append(report.Issues, Issue{Row: row, Pointer: pointer, Code: code})
			} else {
				report.Truncated = true
			}
		}
		for i, rule := range v.rules {
			value, exists := lookup(record, rule.tokens)
			if !exists {
				if rule.Required {
					add(rule.Pointer, "missing")
				}
				continue
			}
			if value == nil {
				if !rule.AllowNull {
					add(rule.Pointer, "null")
				}
				continue
			}
			if !kindMatches(value, rule.Kind) {
				add(rule.Pointer, "type")
				continue
			}
			if s, ok := value.(string); ok {
				if utf8.RuneCountInString(s) < rule.MinLength {
					add(rule.Pointer, "too-short")
				}
				if rule.pattern != nil && !rule.pattern.MatchString(s) {
					add(rule.Pointer, "pattern")
				}
				if rule.Kind == "http-url" {
					u, err := url.Parse(s)
					if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
						add(rule.Pointer, "url")
					}
				}
			}
			if array, ok := value.([]any); ok && len(array) < rule.MinLength {
				add(rule.Pointer, "too-short")
			}
			if rule.Unique {
				encoded, err := json.Marshal(value)
				if err != nil {
					add(rule.Pointer, "unencodable")
					continue
				}
				key := string(encoded)
				// Numerically equivalent number spellings share a uniqueness key.
				if rule.Kind == "number" || rule.Kind == "integer" {
					if r, ok := new(big.Rat).SetString(key); ok {
						key = r.RatString()
					}
				}
				if unique[i][key] {
					add(rule.Pointer, "duplicate")
				} else {
					unique[i][key] = true
				}
			}
		}
		if invalid {
			report.InvalidRows++
		}
	}
	return report
}
