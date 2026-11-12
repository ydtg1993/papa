package breaker

import (
	"testing"
	"time"
)

func TestDetectorConfigValidationAndTinyWindow(t *testing.T) {
	for _, threshold := range []int{0, -1} {
		if Validate(Config{Enabled: true, Threshold: threshold}) == nil {
			t.Fatal("enabled breaker requires positive threshold")
		}
	}
	for _, window := range []time.Duration{0, time.Nanosecond, time.Minute} {
		d := NewDetector(Config{Enabled: true, Threshold: 1, Window: window})
		at := time.Now()
		got := d.Observe(Outcome{TaskID: 1, At: at})
		if got.Rule != "hard" || got.Failures != 1 {
			t.Fatalf("tiny/default window: %+v", got)
		}
		d.ResetEpoch(1)
		if d.Snapshot(at).Failures != 0 {
			t.Fatal("reset did not clear window")
		}
	}
	if got := NewDetector(Config{}).Observe(Outcome{TaskID: 1}); got.Rule != "" || got.Failures != 0 {
		t.Fatal("disabled detector counted failures")
	}
}
