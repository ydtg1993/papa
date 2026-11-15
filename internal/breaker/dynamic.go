package breaker

import (
	"fmt"
	"sort"
	"time"
)

// ValidateExplicit accepts only fully expanded database policies, without defaults.
func ValidateExplicit(c Config) error {
	if c.Window <= 0 || c.Window > 24*time.Hour || c.Threshold <= 0 || c.Threshold > 100000 {
		return fmt.Errorf("breaker window must be >0 and <=24h; threshold must be 1..100000")
	}
	if c.Policy != EffectivePolicy(c.Policy) {
		return fmt.Errorf("breaker policy must contain every effective field; initialize execution_policy explicitly")
	}
	return Validate(c)
}

// UpdateConfig preserves ordered evidence and does not perform detection.
func (d *Detector) UpdateConfig(c Config, now time.Time) error {
	if err := ValidateExplicit(c); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Before(d.last) {
		now = d.last
	}
	// Overflow evidence has an original occurrence time, just like stored rows.
	// Changing the window must not retain an old window's expiry indefinitely.
	if !d.overflowUntil.IsZero() && now.Before(d.overflowUntil) {
		occurred := d.overflowUntil.Add(-max(d.cfg.Window, d.policy.ShortWindow))
		d.overflowUntil = occurred.Add(max(c.Window, c.Policy.ShortWindow))
	} else {
		d.overflowUntil = time.Time{}
	}
	d.cfg, d.policy = c, c.Policy
	d.prune(now)
	trim := func(rows []Outcome) []Outcome {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
		if len(rows) > c.Policy.Capacity {
			rows = append([]Outcome(nil), rows[len(rows)-c.Policy.Capacity:]...)
		}
		return rows
	}
	d.failures, d.successes = trim(d.failures), trim(d.successes)
	return nil
}

// AdvanceEpoch invalidates late task results without clearing statistical evidence.
func (d *Detector) AdvanceEpoch(epoch uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch = epoch
	for i := range d.failures {
		d.failures[i].Epoch = epoch
	}
	for i := range d.successes {
		d.successes[i].Epoch = epoch
	}
}
