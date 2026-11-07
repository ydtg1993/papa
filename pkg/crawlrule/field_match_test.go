package crawlrule

import (
	"encoding/json"
	"testing"
)

func TestCountMatchingFieldsPreservesExactValues(t *testing.T) {
	records := []map[string]any{{"null": nil, "id": json.Number("9007199254740993"), "escaped/key": map[string]any{"~": true}}}
	fields := []FieldExpectation{{Pointer: "/null", Value: nil}, {Pointer: "/missing", Value: nil}, {Pointer: "/id", Value: json.Number("9007199254740993")}, {Pointer: "/id", Value: json.Number("9007199254740992")}, {Pointer: "/escaped~1key/~0", Value: true}, {Row: 1, Pointer: "/id", Value: json.Number("9007199254740993")}, {Pointer: "broken", Value: true}}
	if n := CountMatchingFields(records, fields); n != 3 {
		t.Fatalf("exact assertions matched=%d want 3", n)
	}
}
