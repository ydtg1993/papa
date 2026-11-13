package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraceStepPreservesOriginalValueLayout(t *testing.T) {
	// Intentional positional literal protects the original public struct layout.
	step := TraceStep{1, 2, "parse", "ok", "", "", "", time.Second, time.Time{}}
	data, err := json.Marshal(TraceExecutionStep{TraceStep: step, ExecutionID: "run", Truncated: true})
	if err != nil || !strings.Contains(string(data), `"execution_id":"run"`) || !strings.Contains(string(data), `"attempt":1`) || strings.Contains(string(data), `"TraceStep"`) {
		t.Fatalf("extended snapshot must retain flat JSON: %s, %v", data, err)
	}
}
