package breaker

import (
	"fmt"
	"testing"
	"time"
)

func detectorFixture() (*Detector, time.Time) {
	return NewDetector(Config{Enabled: true, Window: time.Minute, Threshold: 10,
		Policy: Policy{ShortWindow: 10 * time.Second, FastThreshold: 3, MixedThreshold: 5}}), time.Unix(1000, 0)
}

func terminal(id int64, at time.Time, fingerprint string) Outcome {
	return Outcome{TaskID: id, Round: "0:0", Site: "synthetic", Stage: "detail", At: at, Category: "structure", Fingerprint: fingerprint}
}

func TestDetectorFastUsesDistinctTasksAndTaskRoundDedup(t *testing.T) {
	d, now := detectorFixture()
	o := terminal(1, now, "title")
	for i := 0; i < 20; i++ {
		d.Observe(o)
	}
	o.Round = "1:0"
	got := d.Observe(o)
	if got.Failures != 2 || got.DistinctTasks != 1 || got.Rule != "" {
		t.Fatalf("round dedup/distinct: %+v", got)
	}
	d.Observe(terminal(2, now, "title"))
	got = d.Observe(terminal(3, now, "title"))
	if got.Rule != "fast" || got.Stage != "detail" || got.Failures != 4 {
		t.Fatalf("fast: %+v", got)
	}
	for id := int64(4); id <= 6; id++ {
		o = terminal(id, now, "title")
		o.Stage = "list"
		got = d.Observe(o)
	}
	if got.Rule != "fast" || got.Stage != "" {
		t.Fatalf("multiple affected stages must protect site: %+v", got)
	}
}

func TestDetectorMixedHardAndSuccessesDoNotCancelFailures(t *testing.T) {
	d, now := detectorFixture()
	var got Decision
	for id := int64(1); id <= 10; id++ {
		got = d.Observe(terminal(id, now, fmt.Sprint(id)))
		if id == 5 && got.Rule != "mixed" {
			t.Fatalf("mixed: %+v", got)
		}
		o := terminal(100+id, now, "")
		o.Success = true
		got = d.Observe(o)
	}
	if got.Rule != "hard" || got.Failures != 10 || got.Successes != 10 {
		t.Fatalf("hard ceiling: %+v", got)
	}
}

func TestDetectorWindowsEpochAndLateEvents(t *testing.T) {
	d, now := detectorFixture()
	for id := int64(1); id <= 3; id++ {
		d.Observe(terminal(id, now, "title"))
	}
	got := d.Snapshot(now.Add(10 * time.Second))
	if got.Rule != "" || got.Failures != 3 || got.ShortFailures != 0 {
		t.Fatalf("short cutoff: %+v", got)
	}
	d.Snapshot(now.Add(time.Minute))
	got = d.Observe(terminal(4, now, "title"))
	if got.Failures != 0 {
		t.Fatalf("late event resurrected expired window: %+v", got)
	}
	d.ResetEpoch(7)
	o := terminal(5, now.Add(time.Hour), "title")
	o.Epoch = 6
	d.Observe(o)
	o = terminal(6, now, "title")
	o.Epoch = 7
	got = d.Observe(o)
	if got.Epoch != 7 || got.Failures != 1 {
		t.Fatalf("old epoch advanced recovery clock: %+v", got)
	}
}

func TestDetectorCapacityProtectsInsteadOfEvictingFailures(t *testing.T) {
	d := NewDetector(Config{Enabled: true, Window: time.Minute, Threshold: 20, Policy: Policy{Capacity: 3, FastThreshold: 3, MixedThreshold: 3}})
	now := time.Unix(1000, 0)
	for id := int64(1); id <= 3; id++ {
		d.Observe(terminal(id, now, fmt.Sprint(id)))
	}
	for id := int64(100); id <= 110; id++ {
		o := terminal(id, now, "")
		o.Success = true
		d.Observe(o)
	}
	got := d.Snapshot(now)
	if got.Failures != 3 || !got.Limited {
		t.Fatalf("successes evicted failures: %+v", got)
	}
	got = d.Observe(terminal(4, now, "four"))
	if got.Rule != "capacity" || !got.Limited || got.Failures != 3 {
		t.Fatalf("overflow not protected: %+v", got)
	}
	if got = d.Snapshot(now.Add(time.Minute)); got.Rule != "" || got.Failures != 0 {
		t.Fatalf("overflow did not expire: %+v", got)
	}
}

func TestDetectorAnonymousCallsRemainIndependent(t *testing.T) {
	d, now := detectorFixture()
	for i := 0; i < 3; i++ {
		d.Observe(Outcome{At: now, Fingerprint: "x"})
	}
	if got := d.Snapshot(now); got.Rule != "fast" || got.DistinctTasks != 3 {
		t.Fatalf("anonymous: %+v", got)
	}
}

func TestPolicyDefaultsAndInvalidBounds(t *testing.T) {
	if err := ValidatePolicy(Policy{}); err != nil {
		t.Fatal(err)
	}
	p := EffectivePolicy(Policy{})
	if p.FastThreshold != 5 || p.MixedThreshold != 8 || p.Capacity != 4096 || p.MaxRecovery != 5*time.Minute {
		t.Fatalf("defaults: %+v", p)
	}
	for _, p := range []Policy{{ShortWindow: -1}, {MaxProbes: -1}, {Capacity: 4}, {MaxRecovery: time.Second}, {RampConcurrency: 100001}} {
		if err := ValidatePolicy(p); err == nil {
			t.Fatalf("accepted invalid: %+v", p)
		}
	}
}
