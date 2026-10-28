package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTruncateError(t *testing.T) {
	if got := TruncateError("x"); got != "x" {
		t.Fatalf("short error changed: %q", got)
	}
	long := strings.Repeat("a", maxErrorLen+100)
	got := TruncateError(long)
	if len(got) != maxErrorLen {
		t.Fatalf("truncate length = %d, want %d", len(got), maxErrorLen)
	}
}

func TestEventJSONOmitsZeroFields(t *testing.T) {
	ev := Event{Type: TypeRunStarted, RunID: "r-1", TS: time.Unix(0, 0).UTC()}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"type":"run_started"`) {
		t.Fatalf("missing type: %s", s)
	}
	if strings.Contains(s, `"task_id"`) || strings.Contains(s, `"site"`) {
		t.Fatalf("zero fields should be omitted: %s", s)
	}
}
