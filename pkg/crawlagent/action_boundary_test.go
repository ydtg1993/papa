package crawlagent

import (
	"strings"
	"testing"
)

func TestActionsRejectAmbiguousJSONAndCrossToolArguments(t *testing.T) {
	for _, raw := range []string{
		`{"tool":"publish","tool":"finish"}`,
		`{"tool":"finish","note":null}`,
		`{"tool":"finish","url":"https://example.test"}`,
		`{"tool":"submit_candidate","stage":"catalog","rule":{"schema_version":1,"rows":"article","rows":"body"}}`,
		`{"tool":"finish"} {"tool":"finish"}`,
		strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66),
	} {
		if _, err := parseAction(raw); err == nil {
			t.Fatalf("ambiguous action accepted: %s", raw)
		}
	}
	if _, err := parseAction(`{"tool":"read_evidence","evidence_id":"latest","limit":1024}`); err != nil {
		t.Fatal(err)
	}
}
