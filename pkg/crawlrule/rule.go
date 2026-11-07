// Package crawlrule compiles and verifies declarative, offline extraction rules.
// It performs no HTTP requests, JavaScript execution, or automatic publication.
package crawlrule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/cascadia"
	"github.com/ydtg1993/papa/v3/pkg/extract"
)

const (
	SchemaVersion       = 1
	DefaultMaxRows      = 1000
	DefaultMaxBodyBytes = 4 << 20
	DefaultMaxIssues    = 100
	DefaultMaxEvidence  = 10000
	// Hard limits also apply to explicitly configured budgets.
	MaxRowsLimit       = 100000
	MaxBodyBytesLimit  = 64 << 20
	MaxIssuesLimit     = 10000
	MaxEvidenceLimit   = 100000
	MaxFieldsLimit     = 256
	MaxTransformsLimit = 16
	MaxRuleBytes       = 1 << 20
	MaxNextCursorBytes = 16 << 10
)

var (
	ErrNoRecords       = errors.New("crawlrule: no records extracted")
	ErrHTTPStatus      = errors.New("crawlrule: unsuccessful HTTP status")
	ErrValidation      = errors.New("crawlrule: field validation failed")
	ErrTransform       = errors.New("crawlrule: field transform failed")
	ErrBodyTooLarge    = errors.New("crawlrule: response exceeds body budget")
	ErrNextURL         = errors.New("crawlrule: invalid or out-of-scope next URL")
	ErrNextCursor      = errors.New("crawlrule: invalid or oversized next cursor")
	ErrVersionConflict = errors.New("crawlrule: version already registered")
	ErrVersionNotFound = errors.New("crawlrule: version not found")
	ErrNoFixtures      = errors.New("crawlrule: explicit fixtures required")
	ErrFixtureFailed   = errors.New("crawlrule: fixture verification failed")
	ErrNoActiveRule    = errors.New("crawlrule: no active rule")
	ErrNoRollback      = errors.New("crawlrule: no prior verified activation")
)

// Rule is the JSON-serializable schema. Rows is a CSS selector for HTML and a
// JSON Pointer for JSON. An empty JSON pointer selects the root value.
type Rule struct {
	SchemaVersion int     `json:"schema_version"`
	ID            string  `json:"id"`
	Version       string  `json:"version"`
	Format        string  `json:"format"` // html or json
	Rows          string  `json:"rows"`
	Fields        []Field `json:"fields"`
	Next          *Next   `json:"next,omitempty"`
	MaxRows       int     `json:"max_rows,omitempty"`
	MaxBodyBytes  int     `json:"max_body_bytes,omitempty"`
	MaxIssues     int     `json:"max_issues,omitempty"`
	MaxEvidence   int     `json:"max_evidence,omitempty"`
}

// Field produces one top-level record key. HTML selects the first matching
// descendant (or the row itself for an empty selector), reads text unless
// Attribute is set, and trims surrounding whitespace. JSON extraction preserves
// types. Transforms then run in order, before validation, for either format.
type Field struct {
	Name       string      `json:"name"`
	Selector   string      `json:"selector,omitempty"`
	Attribute  string      `json:"attribute,omitempty"`
	Pointer    string      `json:"pointer,omitempty"`
	Validation Validation  `json:"validation,omitempty"`
	Transforms []Transform `json:"transforms,omitempty"`
}

// Validation applies pkg/extract.Validator to this field's output key.
// Kind defaults to the last transform's output kind, or string for untransformed
// HTML and any for untransformed JSON. Required checks presence;
// use MinLength: 1 as well when a required string must not be empty.
type Validation struct {
	Kind      string `json:"kind,omitempty"`
	Required  bool   `json:"required,omitempty"`
	AllowNull bool   `json:"allow_null,omitempty"`
	MinLength int    `json:"min_length,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Unique    bool   `json:"unique,omitempty"`
}

// Next selects pagination from the whole document, not from an extracted row.
// Mode defaults to url. Cursor mode is JSON-only and selects an opaque string
// of at most MaxNextCursorBytes bytes; it forbids AllowExternal. Missing, null or
// empty cursors mean no next page; nonempty cursors retain whitespace verbatim.
// HTML requires Selector and defaults Attribute to href. JSON uses Pointer.
// Missing, null or blank links mean no next page. The final response URL is the
// resolution base; HTML <base> elements are deliberately ignored.
type Next struct {
	Mode          string `json:"mode,omitempty"` // url or cursor
	Selector      string `json:"selector,omitempty"`
	Attribute     string `json:"attribute,omitempty"`
	Pointer       string `json:"pointer,omitempty"`
	AllowExternal bool   `json:"allow_external,omitempty"`
}

type compiledField struct {
	field    Field
	selector cascadia.Selector
	tokens   []string
}

// Compiled is immutable and safe for concurrent evaluation. Rule returns an
// independent snapshot; no caller-owned slices or pointers are retained.
type Compiled struct {
	rule         Rule
	rowsSelector cascadia.Selector
	rowsTokens   []string
	fields       []compiledField
	nextSelector cascadia.Selector
	nextTokens   []string
	validator    *extract.Validator
}

func cloneRule(rule Rule) Rule {
	rule.Fields = append([]Field(nil), rule.Fields...)
	for i := range rule.Fields {
		rule.Fields[i].Transforms = append([]Transform(nil), rule.Fields[i].Transforms...)
	}
	if rule.Next != nil {
		next := *rule.Next
		rule.Next = &next
	}
	return rule
}

func (c *Compiled) Rule() Rule { return cloneRule(c.rule) }

// Parse decodes exactly one schema with no unknown keys before compiling it.
// Use this instead of json.Unmarshal when loading rule files.
func Parse(data []byte) (*Compiled, error) {
	if len(data) > MaxRuleBytes {
		return nil, fmt.Errorf("crawlrule: rule exceeds %d bytes", MaxRuleBytes)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var rule Rule
	if err := d.Decode(&rule); err != nil {
		return nil, fmt.Errorf("crawlrule: decode rule: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("crawlrule: expected exactly one JSON rule")
	}
	return Compile(rule)
}

// Compile validates a version-1 rule and fixes all defaults in its snapshot.
func Compile(rule Rule) (*Compiled, error) {
	rule = cloneRule(rule)
	if rule.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("crawlrule: unsupported schema_version %d", rule.SchemaVersion)
	}
	for key, value := range map[string]string{"id": rule.ID, "version": rule.Version} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 128 {
			return nil, fmt.Errorf("crawlrule: %s must be nonblank, trimmed, and at most 128 bytes", key)
		}
	}
	if rule.Format != "html" && rule.Format != "json" {
		return nil, fmt.Errorf("crawlrule: unsupported format %q", rule.Format)
	}
	if len(rule.Fields) == 0 || len(rule.Fields) > MaxFieldsLimit {
		return nil, fmt.Errorf("crawlrule: field count must be between 1 and %d", MaxFieldsLimit)
	}
	for _, budget := range []struct {
		name string
		ptr  *int
		def  int
		max  int
	}{
		{"max_rows", &rule.MaxRows, DefaultMaxRows, MaxRowsLimit},
		{"max_body_bytes", &rule.MaxBodyBytes, DefaultMaxBodyBytes, MaxBodyBytesLimit},
		{"max_issues", &rule.MaxIssues, DefaultMaxIssues, MaxIssuesLimit},
		{"max_evidence", &rule.MaxEvidence, DefaultMaxEvidence, MaxEvidenceLimit},
	} {
		if *budget.ptr < 0 || *budget.ptr > budget.max {
			return nil, fmt.Errorf("crawlrule: %s outside supported budget", budget.name)
		}
		if *budget.ptr == 0 {
			*budget.ptr = budget.def
		}
	}
	c := &Compiled{}
	var err error
	if rule.Format == "html" {
		if strings.TrimSpace(rule.Rows) == "" {
			return nil, fmt.Errorf("crawlrule: HTML rows selector required")
		}
		c.rowsSelector, err = cascadia.Compile(rule.Rows)
	} else {
		c.rowsTokens, err = parsePointer(rule.Rows)
	}
	if err != nil {
		return nil, fmt.Errorf("crawlrule: rows: %w", err)
	}
	seen := map[string]bool{}
	validations := make([]extract.FieldRule, 0, len(rule.Fields))
	for i, field := range rule.Fields {
		if field.Name == "" || strings.TrimSpace(field.Name) != field.Name || len(field.Name) > 128 || seen[field.Name] {
			return nil, fmt.Errorf("crawlrule: field names must be unique, trimmed, and 1..128 bytes")
		}
		seen[field.Name] = true
		kind, err := compileTransforms(field.Transforms)
		if err != nil {
			return nil, fmt.Errorf("crawlrule: field %q: %w", field.Name, err)
		}
		if field.Validation.Kind == "" {
			field.Validation.Kind = "any"
			if rule.Format == "html" {
				field.Validation.Kind = "string"
			}
			if kind != "" {
				field.Validation.Kind = kind
			}
		}
		cf := compiledField{field: field}
		if rule.Format == "html" {
			if field.Pointer != "" {
				return nil, fmt.Errorf("crawlrule: HTML field cannot specify pointer")
			}
			if field.Selector != "" {
				cf.selector, err = cascadia.Compile(field.Selector)
			}
		} else {
			if field.Selector != "" || field.Attribute != "" {
				return nil, fmt.Errorf("crawlrule: JSON field cannot specify selector or attribute")
			}
			cf.tokens, err = parsePointer(field.Pointer)
		}
		if err != nil {
			return nil, fmt.Errorf("crawlrule: field %q: %w", field.Name, err)
		}
		rule.Fields[i] = field
		c.fields = append(c.fields, cf)
		v := field.Validation
		validations = append(validations, extract.FieldRule{
			Pointer: "/" + strings.ReplaceAll(strings.ReplaceAll(field.Name, "~", "~0"), "/", "~1"),
			Kind:    v.Kind, Required: v.Required, AllowNull: v.AllowNull,
			MinLength: v.MinLength, Pattern: v.Pattern, Unique: v.Unique,
		})
	}
	if rule.Next != nil {
		if rule.Next.Mode == "" {
			rule.Next.Mode = "url"
		}
		if rule.Next.Mode != "url" && rule.Next.Mode != "cursor" {
			return nil, fmt.Errorf("crawlrule: unsupported next mode %q", rule.Next.Mode)
		}
		if rule.Next.Mode == "cursor" && (rule.Format != "json" || rule.Next.AllowExternal) {
			return nil, fmt.Errorf("crawlrule: cursor next requires JSON format and forbids allow_external")
		}
		if rule.Format == "html" {
			if rule.Next.Pointer != "" || strings.TrimSpace(rule.Next.Selector) == "" {
				return nil, fmt.Errorf("crawlrule: HTML next requires selector and forbids pointer")
			}
			if rule.Next.Attribute == "" {
				rule.Next.Attribute = "href"
			}
			c.nextSelector, err = cascadia.Compile(rule.Next.Selector)
		} else {
			if rule.Next.Selector != "" || rule.Next.Attribute != "" {
				return nil, fmt.Errorf("crawlrule: JSON next cannot specify selector or attribute")
			}
			c.nextTokens, err = parsePointer(rule.Next.Pointer)
		}
		if err != nil {
			return nil, fmt.Errorf("crawlrule: next: %w", err)
		}
	}
	c.validator, err = extract.NewValidator(validations)
	if err != nil {
		return nil, fmt.Errorf("crawlrule: validation: %w", err)
	}
	c.rule = rule
	return c, nil
}
