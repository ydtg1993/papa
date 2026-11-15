package breaker

import (
	"testing"
	"time"
)

func TestDynamicPolicyPreservesOrderedEvidenceWithoutTrigger(t *testing.T) {
	now := time.Now()
	c := EffectiveConfig(Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: Policy{FastThreshold: 100, MixedThreshold: 100, Capacity: 100}})
	d := NewDetector(c)
	for i := 0; i < 6; i++ {
		d.Observe(Outcome{TaskID: int64(i + 1), Round: "0", At: now.Add(-time.Duration(6-i) * time.Second)})
	}
	c.Policy.FastThreshold = 2
	c.Policy.MixedThreshold = 2
	c.Policy.Capacity = 3
	c.Enabled = false
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	rows := d.Checkpoint().Failures
	if len(rows) != 3 || rows[0].TaskID != 4 || rows[2].TaskID != 6 {
		t.Fatalf("not latest ordered evidence: %+v", rows)
	}
	d.Observe(Outcome{TaskID: 7, Round: "0", At: now, Success: true})
	if got := d.Snapshot(now); got.Rule != "" || got.Failures != 3 || got.Successes != 1 {
		t.Fatalf("disabled detection=%+v", got)
	}
	c.Enabled = true
	c.Policy.Capacity = 100
	c.Window = time.Second
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	c.Policy.ShortWindow = time.Second
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	if len(d.Checkpoint().Failures) != 0 {
		t.Fatal("shrunk window retained expired evidence")
	}
	c.Window = time.Hour
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	if len(d.Checkpoint().Failures) != 0 {
		t.Fatal("expanded window resurrected evidence")
	}
}

func TestDynamicWindowFiltersOverflowByOccurrenceTime(t *testing.T) {
	now := time.Now()
	c := EffectiveConfig(Config{Enabled: true, Threshold: 100, Window: time.Hour, Policy: Policy{FastThreshold: 1, MixedThreshold: 1, Capacity: 1}})
	d := NewDetector(c)
	d.Observe(Outcome{TaskID: 1, At: now.Add(-time.Minute)})
	d.Observe(Outcome{TaskID: 2, At: now.Add(-time.Minute)})
	if d.Snapshot(now).Rule != "capacity" {
		t.Fatal("missing overflow evidence")
	}
	c.Window = time.Second
	c.Policy.ShortWindow = time.Second
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	if d.Snapshot(now).Rule != "" {
		t.Fatal("expired overflow survived window shrink")
	}
	c.Window = time.Hour
	if err := d.UpdateConfig(c, now); err != nil {
		t.Fatal(err)
	}
	if d.Snapshot(now).Rule != "" {
		t.Fatal("expanded window resurrected expired overflow")
	}
}
