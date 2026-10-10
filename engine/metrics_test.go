package engine

import (
	"testing"

	"github.com/ydtg1993/papa/v3/internal/metrics"
)

func TestEngineQueueMetrics(t *testing.T) {
	e := &Engine{metrics: metrics.New()}
	e.spilled = map[string][]*Task{
		"a": {{}, {}},
		"b": {{}},
	}
	e.spilledCount.Store(10)
	e.recoveredCount.Store(5)
	e.ensureErrorQueues()
	e.errorQueue(errorQueueKey("")).retried.Store(3)
	e.RecordMetric("custom", 42)

	got := e.GetMetrics()

	if got["queue_spilled"] != int64(10) {
		t.Fatalf("queue_spilled = %v, want 10", got["queue_spilled"])
	}
	if got["queue_spill_backlog"] != 3 {
		t.Fatalf("queue_spill_backlog = %v, want 3", got["queue_spill_backlog"])
	}
	if got["recover_total"] != int64(5) {
		t.Fatalf("recover_total = %v, want 5", got["recover_total"])
	}
	if got["error_retry_total"] != int64(3) {
		t.Fatalf("error_retry_total = %v, want 3", got["error_retry_total"])
	}
	if got["custom"] != 42 {
		t.Fatalf("custom = %v, want 42 (业务指标应保留)", got["custom"])
	}
}
