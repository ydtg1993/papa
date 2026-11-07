package crawlrule

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/ydtg1993/papa/v3/pkg/discovery"
	"github.com/ydtg1993/papa/v3/pkg/extract"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// Result owns its records. Truncated means the row budget was exceeded; the
// validation report covers retained records only. A body over budget is rejected
// whole, without parsing a prefix. Validation.Truncated separately caps issues.
// EvidenceTruncated only reports omitted provenance entries; all retained
// records still undergo every configured transform and validation.
type Result struct {
	Records           []map[string]any `json:"records"`
	Evidence          []FieldEvidence  `json:"evidence,omitempty"`
	NextURL           string           `json:"next_url,omitempty"`
	NextCursor        string           `json:"next_cursor,omitempty"`
	Validation        extract.Report   `json:"validation"`
	Truncated         bool             `json:"truncated"`
	EvidenceTruncated bool             `json:"evidence_truncated"`
}

// FieldEvidence identifies each retained record's configured field and source.
// Record is zero-based. Present distinguishes missing fields from explicit null.
// Steps contains successfully applied transforms; null and missing fields have
// no steps. Evidence deliberately excludes field values and response bodies.
type FieldEvidence struct {
	Record    int      `json:"record"`
	Field     string   `json:"field"`
	Selector  string   `json:"selector,omitempty"`
	Attribute string   `json:"attribute,omitempty"`
	Pointer   string   `json:"pointer,omitempty"`
	Present   bool     `json:"present"`
	SourceURL string   `json:"source_url"`
	Steps     []string `json:"steps,omitempty"`
}

// Evaluate processes a caller-owned, complete offline response. Non-2xx status,
// malformed JSON, non-array JSON rows, zero rows and wholly empty records fail.
// Validation failure returns records/report together with ErrValidation. Any
// error means the result must not be published as accepted crawl data.
func (c *Compiled) Evaluate(response *htmlfetch.Response) (Result, error) {
	var out Result
	if c == nil || c.validator == nil {
		return out, fmt.Errorf("crawlrule: compiled rule required")
	}
	if response == nil || response.URL == nil {
		return out, fmt.Errorf("crawlrule: response and final URL required")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return out, fmt.Errorf("%w: response status %d is not successful", ErrHTTPStatus, response.StatusCode)
	}
	return c.evaluate(response.URL.String(), response.Body)
}

// EvaluateRendered evaluates HTML obtained from an already rendered browser
// page. rawURL must be the page's final HTTP(S) URL. The HTTP status is unknown:
// this method neither fabricates a successful status nor checks it. Callers with
// an HTTP response should use Evaluate for its status check. JSON rules are not
// supported because a rendered DOM cannot preserve the original JSON response.
func (c *Compiled) EvaluateRendered(rawURL string, body []byte) (Result, error) {
	if c == nil || c.validator == nil {
		return Result{}, fmt.Errorf("crawlrule: compiled rule required")
	}
	if c.rule.Format != "html" {
		return Result{}, fmt.Errorf("crawlrule: rendered evaluation requires HTML format")
	}
	return c.evaluate(rawURL, body)
}

func (c *Compiled) evaluate(rawURL string, body []byte) (Result, error) {
	var out Result
	base, err := discovery.CanonicalURL(rawURL, "")
	if err != nil {
		return out, fmt.Errorf("crawlrule: response URL must be absolute HTTP(S) without userinfo")
	}
	if len(body) > c.rule.MaxBodyBytes {
		return out, ErrBodyTooLarge
	}
	var next string
	if c.rule.Format == "html" {
		next, err = c.evaluateHTML(body, &out)
	} else {
		next, err = c.evaluateJSON(body, &out)
	}
	if err != nil {
		return out, err
	}
	if err := c.transformRecords(base, &out); err != nil {
		return out, err
	}
	out.Validation = c.validator.Validate(out.Records, c.rule.MaxIssues)
	if len(out.Records) == 0 {
		// Even an empty list must validate pagination; malformed/out-of-scope
		// next links must not become successful negative fixtures.
		if err := c.setNext(base, next, &out); err != nil {
			return out, err
		}
		return out, ErrNoRecords
	}
	if out.Validation.InvalidRows > 0 {
		return out, ErrValidation
	}
	for _, record := range out.Records {
		if len(record) == 0 {
			return out, fmt.Errorf("%w: a row matched no configured fields", ErrNoRecords)
		}
	}
	if err := c.setNext(base, next, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Compiled) setNext(base, next string, out *Result) error {
	if c.rule.Next != nil && c.rule.Next.Mode == "cursor" {
		if len(next) > MaxNextCursorBytes {
			return ErrNextCursor
		}
		out.NextCursor = next
	} else if strings.TrimSpace(next) != "" {
		var err error
		out.NextURL, err = discovery.CanonicalURL(base, next)
		if err != nil || out.NextURL == base || (!c.rule.Next.AllowExternal && !discovery.SameOrigin(base, out.NextURL)) {
			out.NextURL = ""
			return ErrNextURL
		}
	}
	return nil
}

func (c *Compiled) transformRecords(base string, out *Result) error {
	for row, record := range out.Records {
		for _, field := range c.fields {
			value, present := record[field.field.Name]
			collectEvidence := len(out.Evidence) < c.rule.MaxEvidence
			var evidence FieldEvidence
			if collectEvidence {
				evidence = FieldEvidence{
					Record: row, Field: field.field.Name, Selector: field.field.Selector,
					Attribute: field.field.Attribute, Pointer: field.field.Pointer,
					Present: present, SourceURL: base,
				}
			} else {
				out.EvidenceTruncated = true
			}
			if present && value != nil {
				for step, transform := range field.field.Transforms {
					transformed, err := transformValue(value, transform, base)
					if err != nil {
						if collectEvidence {
							out.Evidence = append(out.Evidence, evidence)
						}
						return fmt.Errorf("%w: row %d field %q step %d (%s): %w", ErrTransform, row, field.field.Name, step, transform.Op, err)
					}
					value = transformed
					if collectEvidence {
						evidence.Steps = append(evidence.Steps, transform.Op)
					}
				}
				record[field.field.Name] = value
			}
			if collectEvidence {
				out.Evidence = append(out.Evidence, evidence)
			}
		}
	}
	return nil
}

func (c *Compiled) evaluateHTML(body []byte, out *Result) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("crawlrule: parse HTML: %w", err)
	}
	doc.FindMatcher(c.rowsSelector).EachWithBreak(func(_ int, row *goquery.Selection) bool {
		if len(out.Records) == c.rule.MaxRows {
			out.Truncated = true
			return false
		}
		record := make(map[string]any)
		for _, field := range c.fields {
			node := row
			if field.selector != nil {
				node = row.FindMatcher(field.selector).First()
			}
			if node.Length() == 0 {
				continue
			}
			var value string
			if field.field.Attribute != "" {
				var ok bool
				value, ok = node.Attr(field.field.Attribute)
				if !ok {
					continue
				}
			} else {
				value = node.Text()
			}
			record[field.field.Name] = strings.TrimSpace(value)
		}
		out.Records = append(out.Records, record)
		return true
	})
	if c.rule.Next == nil {
		return "", nil
	}
	value, _ := doc.FindMatcher(c.nextSelector).First().Attr(c.rule.Next.Attribute)
	return value, nil
}

func (c *Compiled) evaluateJSON(body []byte, out *Result) (string, error) {
	var root any
	if err := htmlfetch.DecodeJSON(body, &root); err != nil {
		return "", fmt.Errorf("crawlrule: %w", err)
	}
	value, ok := lookup(root, c.rowsTokens)
	if !ok {
		return "", fmt.Errorf("%w: JSON rows pointer not found", ErrNoRecords)
	}
	rows, ok := value.([]any)
	if !ok {
		return "", fmt.Errorf("crawlrule: JSON rows pointer must select an array")
	}
	if len(rows) > c.rule.MaxRows {
		out.Truncated = true
		rows = rows[:c.rule.MaxRows]
	}
	for _, row := range rows {
		record := make(map[string]any)
		for _, field := range c.fields {
			if v, found := lookup(row, field.tokens); found {
				record[field.field.Name] = v
			}
		}
		out.Records = append(out.Records, record)
	}
	if c.rule.Next == nil {
		return "", nil
	}
	value, ok = lookup(root, c.nextTokens)
	if !ok || value == nil {
		return "", nil
	}
	link, ok := value.(string)
	if !ok {
		if c.rule.Next.Mode == "cursor" {
			return "", fmt.Errorf("%w: JSON next pointer must select a string or null", ErrNextCursor)
		}
		return "", fmt.Errorf("%w: JSON next pointer must select a string or null", ErrNextURL)
	}
	return link, nil
}

func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("expected JSON Pointer beginning with /")
	}
	parts := strings.Split(pointer[1:], "/")
	for n, part := range parts {
		for i := 0; i < len(part); i++ {
			if part[i] == '~' {
				if i+1 == len(part) || (part[i+1] != '0' && part[i+1] != '1') {
					return nil, fmt.Errorf("invalid JSON Pointer escape")
				}
				i++
			}
		}
		parts[n] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func lookup(value any, tokens []string) (any, bool) {
	for _, token := range tokens {
		switch node := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = node[token]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(node) || strconv.Itoa(i) != token {
				return nil, false
			}
			value = node[i]
		default:
			return nil, false
		}
	}
	return value, true
}
