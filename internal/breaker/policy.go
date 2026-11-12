package breaker

import (
	"fmt"
	"time"
)

// Policy bounds detection and recovery. Zero values select documented defaults;
// the legacy Window/Threshold remain the independent hard protection line.
type Policy struct {
	ShortWindow     time.Duration `mapstructure:"short_window" json:"short_window"`
	FastThreshold   int           `mapstructure:"fast_threshold" json:"fast_threshold"`
	MixedThreshold  int           `mapstructure:"mixed_threshold" json:"mixed_threshold"`
	ObserveFor      time.Duration `mapstructure:"observe_for" json:"observe_for"`
	Cooldown        time.Duration `mapstructure:"cooldown" json:"cooldown"`
	ProbeTimeout    time.Duration `mapstructure:"probe_timeout" json:"probe_timeout"`
	MaxProbes       int           `mapstructure:"max_probes" json:"max_probes"`
	ProbeInterval   time.Duration `mapstructure:"probe_interval" json:"probe_interval"`
	MaxRecovery     time.Duration `mapstructure:"max_recovery" json:"max_recovery"`
	RampFor         time.Duration `mapstructure:"ramp_for" json:"ramp_for"`
	RampConcurrency int           `mapstructure:"ramp_concurrency" json:"ramp_concurrency"`
	Interval        time.Duration `mapstructure:"interval" json:"interval"`
	Capacity        int           `mapstructure:"capacity" json:"capacity"`
}

func EffectivePolicy(p Policy) Policy {
	if p.ShortWindow == 0 {
		p.ShortWindow = 30 * time.Second
	}
	if p.FastThreshold == 0 {
		p.FastThreshold = 5
	}
	if p.MixedThreshold == 0 {
		p.MixedThreshold = 8
	}
	if p.ObserveFor == 0 {
		p.ObserveFor = 30 * time.Second
	}
	if p.Cooldown == 0 {
		p.Cooldown = 30 * time.Second
	}
	if p.ProbeTimeout == 0 {
		p.ProbeTimeout = 10 * time.Second
	}
	if p.MaxProbes == 0 {
		p.MaxProbes = 3
	}
	if p.ProbeInterval == 0 {
		p.ProbeInterval = 30 * time.Second
	}
	if p.MaxRecovery == 0 {
		p.MaxRecovery = 5 * time.Minute
	}
	if p.RampFor == 0 {
		p.RampFor = 30 * time.Second
	}
	if p.RampConcurrency == 0 {
		p.RampConcurrency = 1
	}
	if p.Interval == 0 {
		p.Interval = time.Second
	}
	if p.Capacity == 0 {
		p.Capacity = 4096
	}
	return p
}

func ValidatePolicy(p Policy) error {
	p = EffectivePolicy(p)
	for name, d := range map[string]time.Duration{"short_window": p.ShortWindow, "observe_for": p.ObserveFor, "cooldown": p.Cooldown, "probe_timeout": p.ProbeTimeout, "probe_interval": p.ProbeInterval, "max_recovery": p.MaxRecovery, "ramp_for": p.RampFor, "interval": p.Interval} {
		if d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("breaker.policy.%s must be > 0 and <= 24h", name)
		}
	}
	for name, n := range map[string]int{"fast_threshold": p.FastThreshold, "mixed_threshold": p.MixedThreshold, "max_probes": p.MaxProbes, "ramp_concurrency": p.RampConcurrency, "capacity": p.Capacity} {
		if n <= 0 || n > 100000 {
			return fmt.Errorf("breaker.policy.%s must be 1..100000", name)
		}
	}
	if p.FastThreshold > p.Capacity || p.MixedThreshold > p.Capacity {
		return fmt.Errorf("breaker.policy.capacity must cover short-window thresholds")
	}
	if p.ProbeTimeout > p.MaxRecovery || p.Cooldown > p.MaxRecovery || p.RampFor > p.MaxRecovery {
		return fmt.Errorf("breaker.policy max_recovery must cover cooldown, probe_timeout and ramp_for")
	}
	return nil
}
