package config

import "time"

// EffectiveInterval returns the scan interval; zero uses the mandatory queue default.
func (c ErrorQueueConfig) EffectiveInterval() time.Duration {
	if c.Interval == 0 {
		return 4 * time.Hour
	}
	return c.Interval
}

// EffectiveInterval also supplies the fallback period for tasks without their own period.
func (c RepeatQueueConfig) EffectiveInterval() time.Duration {
	if c.Interval == 0 {
		return 12 * time.Hour
	}
	return c.Interval
}
