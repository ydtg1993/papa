package rulequality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
)

// Summarize counts the original, pre-deduplication records without retaining any
// values or provenance URLs. Invalid counts distinct field/row validation issues;
// it is not a count of semantically incorrect values. The returned bool means
// incomplete, including validation that never finished or omitted issue details.
func Summarize(rule crawlrule.Rule, result crawlrule.Result) ([]FieldStat, bool) {
	fields := append([]crawlrule.Field(nil), rule.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	incomplete := result.Truncated || result.Validation.Truncated || len(fields) > MaxFields || result.Validation.Rows != len(result.Records) || result.Validation.TotalIssues != len(result.Validation.Issues)
	if len(fields) > MaxFields {
		fields = fields[:MaxFields]
	}
	rows := result.Records
	if len(rows) > crawlrule.MaxRowsLimit {
		rows = rows[:crawlrule.MaxRowsLimit]
		incomplete = true
	}
	out := make([]FieldStat, len(fields))
	byPointer := make(map[string]int, len(fields))
	for i, field := range fields {
		out[i] = FieldStat{Name: field.Name, Kind: summaryKind(rule.Format, field), Required: field.Validation.Required, Rows: len(rows)}
		pointer := "/" + strings.ReplaceAll(strings.ReplaceAll(field.Name, "~", "~0"), "/", "~1")
		if _, exists := byPointer[pointer]; exists {
			incomplete = true
		}
		byPointer[pointer] = i
		for _, row := range rows {
			value, present := row[field.Name]
			if !present {
				continue
			}
			out[i].Present++
			if value == nil {
				out[i].Null++
				continue
			}
			switch value := value.(type) {
			case string:
				if strings.TrimSpace(value) != "" {
					out[i].NonEmpty++
				}
			case []any:
				if len(value) != 0 {
					out[i].NonEmpty++
				}
			case map[string]any:
				if len(value) != 0 {
					out[i].NonEmpty++
				}
			default:
				// Numeric zero and false are real, nonempty values.
				out[i].NonEmpty++
			}
		}
	}
	type issueKey struct{ field, row int }
	seen := make(map[issueKey]bool)
	for _, issue := range result.Validation.Issues {
		i, exists := byPointer[issue.Pointer]
		if !exists || issue.Row < 0 || issue.Row >= len(rows) {
			incomplete = true
			continue
		}
		key := issueKey{i, issue.Row}
		if !seen[key] {
			out[i].Invalid++
			seen[key] = true
		}
	}
	return out, incomplete
}

// FieldSchemaDigest identifies the full, normalized field validation contract.
// Extraction locations and rule version are deliberately excluded: a selector
// fix can be compared while a different validation contract cannot.
func FieldSchemaDigest(rule crawlrule.Rule) string {
	type schemaField struct {
		Name       string               `json:"name"`
		Validation crawlrule.Validation `json:"validation"`
	}
	fields := make([]schemaField, len(rule.Fields))
	for i, field := range rule.Fields {
		validation := field.Validation
		validation.Kind = summaryKind(rule.Format, field)
		fields[i] = schemaField{field.Name, validation}
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	data, _ := json.Marshal(fields)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func summaryKind(format string, field crawlrule.Field) string {
	if field.Validation.Kind != "" {
		return field.Validation.Kind
	}
	kind := "any"
	if format == "html" {
		kind = "string"
	}
	for _, transform := range field.Transforms {
		switch transform.Op {
		case "trim", "date":
			kind = "string"
		case "absolute_url":
			kind = "http-url"
		case "number":
			kind = "number"
		case "bool":
			kind = "bool"
		}
	}
	return kind
}
