package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraceExecutionJSON(t *testing.T) {
	// The current wire format is flat and includes execution metadata.
	step := TraceStep{Attempt: 1, Seq: 2, Step: "parse", Status: "ok", Duration: time.Second}
	data, err := json.Marshal(TraceExecutionStep{TraceStep: step, ExecutionID: "run", Truncated: true})
	if err != nil || !strings.Contains(string(data), `"execution_id":"run"`) || !strings.Contains(string(data), `"attempt":1`) || strings.Contains(string(data), `"TraceStep"`) {
		t.Fatalf("extended snapshot must retain flat JSON: %s, %v", data, err)
	}
}
