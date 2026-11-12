package breaker

import (
	"testing"
	"time"
)

func TestCheckpointRestoresFailureIdentityAndOriginalExpiry(t *testing.T) {
	now := time.Now()
	cfg := Config{Enabled: true, Window: time.Minute, Threshold: 10}
	d := NewDetector(cfg)
	o := Outcome{Site: "fixture", Stage: "detail", TaskID: 1, Round: "0:0", At: now, Category: "structure", Fingerprint: "missing"}
	d.Observe(o)
	c := d.Checkpoint()
	restarted := NewDetector(cfg)
	restarted.Restore(c, 4, now.Add(10*time.Second))
	if got := restarted.Snapshot(now.Add(10 * time.Second)); got.Failures != 1 {
		t.Fatalf("failure lost: %+v", got)
	}
	o.Epoch, o.At = 4, now.Add(11*time.Second)
	if got := restarted.Observe(o); got.Accepted || got.Failures != 1 {
		t.Fatalf("duplicate round counted after restart: %+v", got)
	}
	if got := restarted.Snapshot(now.Add(61 * time.Second)); got.Failures != 0 {
		t.Fatal("restart extended original failure window")
	}
	if restarted.Snapshot(now).Successes != 0 {
		t.Fatal("restart fabricated health proof")
	}
}

func TestCheckpointRestoresCapacityProtection(t *testing.T) {
	now := time.Now()
	cfg := Config{Enabled: true, Window: time.Minute, Threshold: 10, Policy: Policy{Capacity: 2, FastThreshold: 2, MixedThreshold: 2}}
	d := NewDetector(cfg)
	for i := 1; i <= 3; i++ {
		d.Observe(Outcome{TaskID: int64(i), Round: "0", At: now})
	}
	restarted := NewDetector(cfg)
	restarted.Restore(d.Checkpoint(), 2, now.Add(time.Second))
	if got := restarted.Snapshot(now.Add(time.Second)); got.Rule != "capacity" {
		t.Fatalf("overflow protection lost: %+v", got)
	}
}

func TestCheckpointSmallerCapacityKeepsKnownFailuresBounded(t *testing.T) {
	now := time.Now()
	cfg := Config{Enabled: true, Window: time.Minute, Threshold: 10, Policy: Policy{Capacity: 5, FastThreshold: 2, MixedThreshold: 2}}
	d := NewDetector(cfg)
	for i := 1; i <= 4; i++ {
		d.Observe(Outcome{TaskID: int64(i), Round: "0", At: now})
	}
	cfg.Policy.Capacity = 2
	r := NewDetector(cfg)
	r.Restore(d.Checkpoint(), 1, now)
	for i := 5; i <= 20; i++ {
		r.Observe(Outcome{TaskID: int64(i), Round: "0", Epoch: 1, At: now})
	}
	if got := r.Snapshot(now); got.Failures != 4 || got.Rule != "capacity" {
		t.Fatalf("lost known failures or exceeded retained checkpoint: %+v", got)
	}
}
