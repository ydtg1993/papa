package governance

import "time"

// ResourcePatch preserves explicit false/zero values in persisted overrides.
type ResourcePatch struct {
	Enabled               *bool          `json:"enabled,omitempty" mapstructure:"enabled" yaml:"enabled,omitempty"`
	SampleInterval        *time.Duration `json:"sample_interval,omitempty" mapstructure:"sample_interval" yaml:"sample_interval,omitempty"`
	CPUHigh               *float64       `json:"cpu_high,omitempty" mapstructure:"cpu_high" yaml:"cpu_high,omitempty"`
	CPURecover            *float64       `json:"cpu_recover,omitempty" mapstructure:"cpu_recover" yaml:"cpu_recover,omitempty"`
	HighWindows           *int           `json:"high_windows,omitempty" mapstructure:"high_windows" yaml:"high_windows,omitempty"`
	MemoryHigh            *float64       `json:"memory_high,omitempty" mapstructure:"memory_high" yaml:"memory_high,omitempty"`
	MemoryRecover         *float64       `json:"memory_recover,omitempty" mapstructure:"memory_recover" yaml:"memory_recover,omitempty"`
	MinimumAvailableBytes *uint64        `json:"minimum_available_bytes,omitempty" mapstructure:"minimum_available_bytes" yaml:"minimum_available_bytes,omitempty"`
	ShrinkFactor          *float64       `json:"shrink_factor,omitempty" mapstructure:"shrink_factor" yaml:"shrink_factor,omitempty"`
	RecoveryStep          *int           `json:"recovery_step,omitempty" mapstructure:"recovery_step" yaml:"recovery_step,omitempty"`
	RecoveryInterval      *time.Duration `json:"recovery_interval,omitempty" mapstructure:"recovery_interval" yaml:"recovery_interval,omitempty"`
	TaskMax               *int           `json:"task_max,omitempty" mapstructure:"task_max" yaml:"task_max,omitempty"`
	BrowserMax            *int           `json:"browser_max,omitempty" mapstructure:"browser_max" yaml:"browser_max,omitempty"`
}

func MergeResources(base ResourcePolicy, patch ResourcePatch) ResourcePolicy {
	assign(&base.Enabled, patch.Enabled)
	assign(&base.SampleInterval, patch.SampleInterval)
	assign(&base.CPUHigh, patch.CPUHigh)
	assign(&base.CPURecover, patch.CPURecover)
	assign(&base.HighWindows, patch.HighWindows)
	assign(&base.MemoryHigh, patch.MemoryHigh)
	assign(&base.MemoryRecover, patch.MemoryRecover)
	assign(&base.MinimumAvailableBytes, patch.MinimumAvailableBytes)
	assign(&base.ShrinkFactor, patch.ShrinkFactor)
	assign(&base.RecoveryStep, patch.RecoveryStep)
	assign(&base.RecoveryInterval, patch.RecoveryInterval)
	assign(&base.TaskMax, patch.TaskMax)
	assign(&base.BrowserMax, patch.BrowserMax)
	return base
}

// PatchOfPolicy packs a resolved value as a fully explicit independent patch.
func PatchOfPolicy(p Policy) PolicyPatch {
	p = MergePolicy(p, PolicyPatch{})
	r, a := p.Request, p.Request.AutoThrottle
	out := PolicyPatch{Weight: &p.Weight, MinWeight: &p.MinWeight, Pool: &p.Pool, Stages: make(map[string]StagePatch),
		Request: &RequestPatch{Enabled: &r.Enabled, MaxInflight: &r.MaxInflight, RequestsPerSecond: &r.RequestsPerSecond, Burst: &r.Burst, ManualDelay: &r.ManualDelay, RetryAfterDefault: &r.RetryAfterDefault, RetryAfterMax: &r.RetryAfterMax,
			AutoThrottle: &AutoThrottlePatch{Enabled: &a.Enabled, MinDelay: &a.MinDelay, MaxDelay: &a.MaxDelay, TargetConcurrency: &a.TargetConcurrency, Smoothing: &a.Smoothing}}}
	for k, s := range p.Stages {
		out.Stages[k] = StagePatch{MaxConcurrent: &s.MaxConcurrent, Delay: &s.Delay, Pool: &s.Pool}
	}
	return out
}
