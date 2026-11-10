// Package governance provides single-process request and resource admission.
// It has no dependency on the engine, configuration store, or business sites.
package governance

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type Config struct {
	Defaults  Policy                 `json:"defaults" mapstructure:"defaults" yaml:"defaults"`
	Sites     map[string]PolicyPatch `json:"sites,omitempty" mapstructure:"sites" yaml:"sites,omitempty"`
	Resources ResourcePolicy         `json:"resources" mapstructure:"resources" yaml:"resources"`
}

type Policy struct {
	Request   RequestPolicy          `json:"request" mapstructure:"request" yaml:"request"`
	Weight    int                    `json:"weight" mapstructure:"weight" yaml:"weight"`
	MinWeight int                    `json:"min_weight" mapstructure:"min_weight" yaml:"min_weight"`
	Pool      string                 `json:"pool,omitempty" mapstructure:"pool" yaml:"pool,omitempty"`
	Stages    map[string]StagePolicy `json:"stages,omitempty" mapstructure:"stages" yaml:"stages,omitempty"`
}

type RequestPolicy struct {
	Enabled           bool               `json:"enabled" mapstructure:"enabled" yaml:"enabled"`
	MaxInflight       int                `json:"max_inflight" mapstructure:"max_inflight" yaml:"max_inflight"`
	RequestsPerSecond float64            `json:"requests_per_second" mapstructure:"requests_per_second" yaml:"requests_per_second"`
	Burst             int                `json:"burst" mapstructure:"burst" yaml:"burst"`
	ManualDelay       time.Duration      `json:"manual_delay" mapstructure:"manual_delay" yaml:"manual_delay"`
	RetryAfterDefault time.Duration      `json:"retry_after_default" mapstructure:"retry_after_default" yaml:"retry_after_default"`
	RetryAfterMax     time.Duration      `json:"retry_after_max" mapstructure:"retry_after_max" yaml:"retry_after_max"`
	AutoThrottle      AutoThrottlePolicy `json:"autothrottle" mapstructure:"autothrottle" yaml:"autothrottle"`
}

type AutoThrottlePolicy struct {
	Enabled           bool          `json:"enabled" mapstructure:"enabled" yaml:"enabled"`
	MinDelay          time.Duration `json:"min_delay" mapstructure:"min_delay" yaml:"min_delay"`
	MaxDelay          time.Duration `json:"max_delay" mapstructure:"max_delay" yaml:"max_delay"`
	TargetConcurrency float64       `json:"target_concurrency" mapstructure:"target_concurrency" yaml:"target_concurrency"`
	Smoothing         float64       `json:"smoothing" mapstructure:"smoothing" yaml:"smoothing"`
}

// Zero stage concurrency leaves the physical worker capacity unchanged.
type StagePolicy struct {
	MaxConcurrent int           `json:"max_concurrent" mapstructure:"max_concurrent" yaml:"max_concurrent"`
	Delay         time.Duration `json:"delay" mapstructure:"delay" yaml:"delay"`
	Pool          string        `json:"pool,omitempty" mapstructure:"pool" yaml:"pool,omitempty"`
}

// Patches preserve the difference between an absent value and explicit false/0.
type PolicyPatch struct {
	Request   *RequestPatch         `json:"request,omitempty" mapstructure:"request" yaml:"request,omitempty"`
	Weight    *int                  `json:"weight,omitempty" mapstructure:"weight" yaml:"weight,omitempty"`
	MinWeight *int                  `json:"min_weight,omitempty" mapstructure:"min_weight" yaml:"min_weight,omitempty"`
	Pool      *string               `json:"pool,omitempty" mapstructure:"pool" yaml:"pool,omitempty"`
	Stages    map[string]StagePatch `json:"stages,omitempty" mapstructure:"stages" yaml:"stages,omitempty"`
}

type RequestPatch struct {
	Enabled           *bool              `json:"enabled,omitempty" mapstructure:"enabled" yaml:"enabled,omitempty"`
	MaxInflight       *int               `json:"max_inflight,omitempty" mapstructure:"max_inflight" yaml:"max_inflight,omitempty"`
	RequestsPerSecond *float64           `json:"requests_per_second,omitempty" mapstructure:"requests_per_second" yaml:"requests_per_second,omitempty"`
	Burst             *int               `json:"burst,omitempty" mapstructure:"burst" yaml:"burst,omitempty"`
	ManualDelay       *time.Duration     `json:"manual_delay,omitempty" mapstructure:"manual_delay" yaml:"manual_delay,omitempty"`
	RetryAfterDefault *time.Duration     `json:"retry_after_default,omitempty" mapstructure:"retry_after_default" yaml:"retry_after_default,omitempty"`
	RetryAfterMax     *time.Duration     `json:"retry_after_max,omitempty" mapstructure:"retry_after_max" yaml:"retry_after_max,omitempty"`
	AutoThrottle      *AutoThrottlePatch `json:"autothrottle,omitempty" mapstructure:"autothrottle" yaml:"autothrottle,omitempty"`
}

type AutoThrottlePatch struct {
	Enabled           *bool          `json:"enabled,omitempty" mapstructure:"enabled" yaml:"enabled,omitempty"`
	MinDelay          *time.Duration `json:"min_delay,omitempty" mapstructure:"min_delay" yaml:"min_delay,omitempty"`
	MaxDelay          *time.Duration `json:"max_delay,omitempty" mapstructure:"max_delay" yaml:"max_delay,omitempty"`
	TargetConcurrency *float64       `json:"target_concurrency,omitempty" mapstructure:"target_concurrency" yaml:"target_concurrency,omitempty"`
	Smoothing         *float64       `json:"smoothing,omitempty" mapstructure:"smoothing" yaml:"smoothing,omitempty"`
}

type StagePatch struct {
	MaxConcurrent *int           `json:"max_concurrent,omitempty" mapstructure:"max_concurrent" yaml:"max_concurrent,omitempty"`
	Delay         *time.Duration `json:"delay,omitempty" mapstructure:"delay" yaml:"delay,omitempty"`
	Pool          *string        `json:"pool,omitempty" mapstructure:"pool" yaml:"pool,omitempty"`
}

type ResourcePolicy struct {
	Enabled               bool          `json:"enabled" mapstructure:"enabled" yaml:"enabled"`
	SampleInterval        time.Duration `json:"sample_interval" mapstructure:"sample_interval" yaml:"sample_interval"`
	CPUHigh               float64       `json:"cpu_high" mapstructure:"cpu_high" yaml:"cpu_high"`
	CPURecover            float64       `json:"cpu_recover" mapstructure:"cpu_recover" yaml:"cpu_recover"`
	HighWindows           int           `json:"high_windows" mapstructure:"high_windows" yaml:"high_windows"`
	MemoryHigh            float64       `json:"memory_high" mapstructure:"memory_high" yaml:"memory_high"`
	MemoryRecover         float64       `json:"memory_recover" mapstructure:"memory_recover" yaml:"memory_recover"`
	MinimumAvailableBytes uint64        `json:"minimum_available_bytes" mapstructure:"minimum_available_bytes" yaml:"minimum_available_bytes"`
	ShrinkFactor          float64       `json:"shrink_factor" mapstructure:"shrink_factor" yaml:"shrink_factor"`
	RecoveryStep          int           `json:"recovery_step" mapstructure:"recovery_step" yaml:"recovery_step"`
	RecoveryInterval      time.Duration `json:"recovery_interval" mapstructure:"recovery_interval" yaml:"recovery_interval"`
	TaskMax               int           `json:"task_max" mapstructure:"task_max" yaml:"task_max"`
	BrowserMax            int           `json:"browser_max" mapstructure:"browser_max" yaml:"browser_max"`
}

func DefaultConfig() Config {
	return Config{
		Defaults: Policy{Weight: 1, MinWeight: 1, Request: RequestPolicy{
			Burst: 1, RetryAfterDefault: 30 * time.Second, RetryAfterMax: time.Hour,
			AutoThrottle: AutoThrottlePolicy{MinDelay: 100 * time.Millisecond, MaxDelay: 30 * time.Second, TargetConcurrency: 1, Smoothing: .5},
		}},
		Resources: ResourcePolicy{SampleInterval: 2 * time.Second, CPUHigh: 85, CPURecover: 65, HighWindows: 3,
			MemoryHigh: 90, MemoryRecover: 75, ShrinkFactor: .5, RecoveryStep: 1, RecoveryInterval: 30 * time.Second, TaskMax: 32, BrowserMax: 4},
	}
}

func assign[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// MergePolicy returns an independent value. It never retains caller-owned maps.
func MergePolicy(base Policy, patch PolicyPatch) Policy {
	stages := make(map[string]StagePolicy, len(base.Stages)+len(patch.Stages))
	for k, v := range base.Stages {
		stages[k] = v
	}
	base.Stages = stages
	assign(&base.Weight, patch.Weight)
	assign(&base.MinWeight, patch.MinWeight)
	assign(&base.Pool, patch.Pool)
	if p := patch.Request; p != nil {
		r := &base.Request
		assign(&r.Enabled, p.Enabled)
		assign(&r.MaxInflight, p.MaxInflight)
		assign(&r.RequestsPerSecond, p.RequestsPerSecond)
		assign(&r.Burst, p.Burst)
		assign(&r.ManualDelay, p.ManualDelay)
		assign(&r.RetryAfterDefault, p.RetryAfterDefault)
		assign(&r.RetryAfterMax, p.RetryAfterMax)
		if a := p.AutoThrottle; a != nil {
			assign(&r.AutoThrottle.Enabled, a.Enabled)
			assign(&r.AutoThrottle.MinDelay, a.MinDelay)
			assign(&r.AutoThrottle.MaxDelay, a.MaxDelay)
			assign(&r.AutoThrottle.TargetConcurrency, a.TargetConcurrency)
			assign(&r.AutoThrottle.Smoothing, a.Smoothing)
		}
	}
	for k, p := range patch.Stages {
		s := stages[k]
		assign(&s.MaxConcurrent, p.MaxConcurrent)
		assign(&s.Delay, p.Delay)
		assign(&s.Pool, p.Pool)
		stages[k] = s
	}
	return base
}

func (c Config) Effective(site string) Policy { return MergePolicy(c.Defaults, c.Sites[site]) }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (c Config) Validate() error {
	if err := validatePolicy(c.Defaults); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	for site, patch := range c.Sites {
		if strings.TrimSpace(site) == "" || len(site) > 256 {
			return fmt.Errorf("invalid site key")
		}
		if err := validatePolicy(MergePolicy(c.Defaults, patch)); err != nil {
			return fmt.Errorf("site %q: %w", site, err)
		}
	}
	r := c.Resources
	for _, v := range []float64{r.CPUHigh, r.CPURecover, r.MemoryHigh, r.MemoryRecover, r.ShrinkFactor} {
		if !finite(v) {
			return fmt.Errorf("resources: non-finite value")
		}
	}
	if r.SampleInterval < 0 || r.RecoveryInterval < 0 || r.TaskMax < 0 || r.BrowserMax < 0 || r.HighWindows < 0 || r.RecoveryStep < 0 {
		return fmt.Errorf("resources: negative limit")
	}
	if r.CPUHigh < 0 || r.CPUHigh > 100 || r.CPURecover < 0 || r.CPURecover > 100 || r.MemoryHigh < 0 || r.MemoryHigh > 100 || r.MemoryRecover < 0 || r.MemoryRecover > 100 || r.ShrinkFactor < 0 || r.ShrinkFactor >= 1 {
		return fmt.Errorf("resources: thresholds or shrink factor out of range")
	}
	if r.Enabled && (r.SampleInterval <= 0 || r.RecoveryInterval <= 0 || r.TaskMax <= 0 || r.BrowserMax <= 0 || r.HighWindows <= 0 || r.RecoveryStep <= 0 || r.ShrinkFactor <= 0 || r.CPUHigh <= r.CPURecover || r.MemoryHigh <= r.MemoryRecover) {
		return fmt.Errorf("resources: enabled control requires positive capacities, intervals, hysteresis, and shrink factor")
	}
	return nil
}

func validatePolicy(p Policy) error {
	if p.Weight < 0 || p.Weight > 10000 || p.MinWeight < 0 || p.MinWeight > 10000 {
		return fmt.Errorf("weight must be between 0 and 10000")
	}
	if len(p.Pool) > 256 {
		return fmt.Errorf("pool name too long")
	}
	for stage, s := range p.Stages {
		if strings.TrimSpace(stage) == "" || len(stage) > 256 || s.MaxConcurrent < 0 || s.Delay < 0 || len(s.Pool) > 256 {
			return fmt.Errorf("invalid stage policy")
		}
	}
	r, a := p.Request, p.Request.AutoThrottle
	if !finite(r.RequestsPerSecond) || !finite(a.TargetConcurrency) || !finite(a.Smoothing) {
		return fmt.Errorf("request: non-finite value")
	}
	if r.MaxInflight < 0 || r.Burst < 0 || r.RequestsPerSecond < 0 || r.ManualDelay < 0 || r.RetryAfterDefault < 0 || r.RetryAfterMax < 0 || a.MinDelay < 0 || a.MaxDelay < 0 || a.TargetConcurrency < 0 || a.Smoothing < 0 || a.Smoothing > 1 {
		return fmt.Errorf("request: negative or out-of-range parameter")
	}
	if r.RequestsPerSecond > 0 && r.Burst < 1 {
		return fmt.Errorf("request: positive rate requires burst >= 1")
	}
	if a.Enabled && (a.MaxDelay < a.MinDelay || a.MaxDelay <= 0 || a.TargetConcurrency <= 0 || a.Smoothing <= 0) {
		return fmt.Errorf("autothrottle: requires max_delay >= min_delay, positive target and smoothing")
	}
	if r.Enabled && (r.RetryAfterDefault <= 0 || r.RetryAfterMax <= 0 || r.RetryAfterDefault > r.RetryAfterMax) {
		return fmt.Errorf("request: enabled control requires positive retry-after default <= maximum")
	}
	return nil
}
