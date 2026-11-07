package crawlrule

import (
	"encoding/json"

	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// CountMatchingFields counts exact host-authored field assertions for diagnostic
// evaluation. Missing fields never match null; numbers retain JSON precision.
// It does not validate rows or authorize activation/publication. Callers supply
// bounded expectations (as for Verify), never model-generated answer keys.
func CountMatchingFields(records []map[string]any, fields []FieldExpectation) int {
	matched := 0
	for _, f := range fields {
		if f.Row < 0 || f.Row >= len(records) {
			continue
		}
		tokens, err := parsePointer(f.Pointer)
		if err != nil {
			continue
		}
		actual, exists := lookup(records[f.Row], tokens)
		if !exists {
			continue
		}
		raw, err := json.Marshal(f.Value)
		if err != nil {
			continue
		}
		var expected any
		if htmlfetch.DecodeJSON(raw, &expected) == nil && equalJSON(actual, expected) {
			matched++
		}
	}
	return matched
}
