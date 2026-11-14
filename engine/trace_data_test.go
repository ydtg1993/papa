package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

type panicTraceJSON struct{}

func (panicTraceJSON) MarshalJSON() ([]byte, error) { panic("bad marshaler") }
func TestTraceDataPreservesJSONSemantics(t *testing.T) {
	type embedded struct {
		Visible string `json:"visible"`
	}
	type payload struct {
		embedded
		Bytes  []byte          `json:"bytes"`
		Raw    json.RawMessage `json:"raw"`
		Empty  string          `json:"empty,omitempty"`
		Number int             `json:"number,string"`
	}
	value := payload{embedded: embedded{"yes"}, Bytes: []byte("abc"), Raw: json.RawMessage(`{"ok":true}`), Number: 2}
	want, _ := marshalNoHTMLEscape(value)
	got, cut := traceData(value)
	if cut || got != string(want) {
		t.Fatalf("got=%s want=%s truncated=%v", got, want, cut)
	}
	got, _ = traceData(panicTraceJSON{})
	if !strings.Contains(got, "_marshal_error") {
		t.Fatal(got)
	}
}

func TestTraceDataPreflightsEmbeddedAndRawJSON(t *testing.T) {
	type hidden struct{ Visible string }
	type outer struct{ hidden }
	for _, v := range []any{outer{hidden{strings.Repeat("x", maxTraceDataBytes*2)}}, json.RawMessage(`"` + strings.Repeat("x", maxTraceDataBytes*2) + `"`)} {
		got, cut := traceData(v)
		if !cut || got != `{"_truncated":true}` {
			t.Fatalf("unbounded payload %T: len=%d cut=%v", v, len(got), cut)
		}
	}
	ignored := struct {
		Large string `json:"-"`
	}{strings.Repeat("x", maxTraceDataBytes*2)}
	got, cut := traceData(ignored)
	if cut || got != "{}" {
		t.Fatalf("ignored field=%s %v", got, cut)
	}
}
