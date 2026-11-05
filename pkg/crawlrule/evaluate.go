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
type Result struct {
	Records    []map[string]any `json:"records"`
	NextURL    string           `json:"next_url,omitempty"`
	Validation extract.Report   `json:"validation"`
	Truncated  bool             `json:"truncated"`
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
	base, err := discovery.CanonicalURL(response.URL.String(), "")
	if err != nil {
		return out, fmt.Errorf("crawlrule: response URL must be absolute HTTP(S)")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return out, fmt.Errorf("crawlrule: response status %d is not successful", response.StatusCode)
	}
	if len(response.Body) > c.rule.MaxBodyBytes {
		return out, ErrBodyTooLarge
	}
	var next string
	if c.rule.Format == "html" {
		next, err = c.evaluateHTML(response.Body, &out)
	} else {
		next, err = c.evaluateJSON(response.Body, &out)
	}
	if err != nil {
		return out, err
	}
	out.Validation = c.validator.Validate(out.Records, c.rule.MaxIssues)
	if len(out.Records) == 0 {
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
	if strings.TrimSpace(next) != "" {
		out.NextURL, err = discovery.CanonicalURL(base, next)
		if err != nil || out.NextURL == base || (!c.rule.Next.AllowExternal && !discovery.SameOrigin(base, out.NextURL)) {
			out.NextURL = ""
			return out, ErrNextURL
		}
	}
	return out, nil
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
		record := make(map[string]any, len(c.fields))
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
		record := make(map[string]any, len(c.fields))
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
