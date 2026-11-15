// Package executionpolicy defines database-owned execution settings and their versioned control API.
package executionpolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/breakerpolicy"
	"io"
	"time"
)

const Key = "execution_policy"

type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string with units: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err == nil {
		*d = Duration(v)
	}
	return err
}

type Policy struct {
	ShortWindow    Duration `json:"short_window"`
	FastThreshold  int      `json:"fast_threshold"`
	MixedThreshold int      `json:"mixed_threshold"`
	ObserveFor     Duration `json:"observe_for"`
	Cooldown       Duration `json:"cooldown"`
	ProbeTimeout   Duration `json:"probe_timeout"`
	MaxProbes      int      `json:"max_probes"`
	ProbeInterval  Duration `json:"probe_interval"`
	MaxRecovery    Duration `json:"max_recovery"`
	RampFor        Duration `json:"ramp_for"`
	Interval       Duration `json:"interval"`
	Capacity       int      `json:"capacity"`
}

func (p Policy) Native() breakerpolicy.Policy {
	return breakerpolicy.Policy{ShortWindow: time.Duration(p.ShortWindow), FastThreshold: p.FastThreshold, MixedThreshold: p.MixedThreshold, ObserveFor: time.Duration(p.ObserveFor), Cooldown: time.Duration(p.Cooldown), ProbeTimeout: time.Duration(p.ProbeTimeout), MaxProbes: p.MaxProbes, ProbeInterval: time.Duration(p.ProbeInterval), MaxRecovery: time.Duration(p.MaxRecovery), RampFor: time.Duration(p.RampFor), Interval: time.Duration(p.Interval), Capacity: p.Capacity}
}
func policyFrom(p breakerpolicy.Policy) Policy {
	return Policy{Duration(p.ShortWindow), p.FastThreshold, p.MixedThreshold, Duration(p.ObserveFor), Duration(p.Cooldown), Duration(p.ProbeTimeout), p.MaxProbes, Duration(p.ProbeInterval), Duration(p.MaxRecovery), Duration(p.RampFor), Duration(p.Interval), p.Capacity}
}

type Breaker struct {
	Enabled   bool     `json:"enabled"`
	Window    Duration `json:"window"`
	Threshold int      `json:"threshold"`
	Policy    Policy   `json:"policy"`
}

func (b Breaker) Native() config.BreakerConfig {
	return config.BreakerConfig{Enabled: b.Enabled, Window: time.Duration(b.Window), Threshold: b.Threshold, Policy: b.Policy.Native()}
}

type Breakers struct {
	Defaults Breaker            `json:"defaults"`
	Sites    map[string]Breaker `json:"sites"`
}
type Browser struct {
	PoolSize       int      `json:"pool_size"`
	DirectPoolSize int      `json:"direct_pool_size"`
	MaxIdleTime    Duration `json:"max_idle_time"`
}
type Document struct {
	SchemaVersion int      `json:"schema_version"`
	Breaker       Breakers `json:"breaker"`
	Browser       Browser  `json:"browser"`
}

// InitialDocument is an explicit bootstrap example, never a runtime fallback.
func InitialDocument() Document {
	return Document{SchemaVersion: 1, Breaker: Breakers{Defaults: Breaker{Window: Duration(5 * time.Minute), Threshold: 50, Policy: policyFrom(breakerpolicy.EffectivePolicy(breakerpolicy.Policy{}))}, Sites: map[string]Breaker{}}, Browser: Browser{PoolSize: 3, MaxIdleTime: Duration(5 * time.Minute)}}
}
func Decode(raw []byte) (Document, error) {
	var d Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return d, fmt.Errorf("expected one policy document")
	}
	return d, nil
}
func (d Document) Validate(sites []string) error {
	if d.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if d.Browser.PoolSize <= 0 || d.Browser.DirectPoolSize < 0 || d.Browser.MaxIdleTime < 0 {
		return fmt.Errorf("browser requires pool_size > 0, direct_pool_size >= 0, max_idle_time >= 0")
	}
	known := map[string]bool{}
	for _, s := range sites {
		known[s] = true
	}
	check := func(b Breaker) error {
		if b.Window <= 0 || b.Window > Duration(24*time.Hour) || b.Threshold <= 0 || b.Threshold > 100000 {
			return fmt.Errorf("breaker requires threshold 1..100000 and window <= 24h")
		}
		p := b.Policy.Native()
		if p.ShortWindow <= 0 || p.ObserveFor <= 0 || p.Cooldown <= 0 || p.ProbeTimeout <= 0 || p.ProbeInterval <= 0 || p.MaxRecovery <= 0 || p.RampFor <= 0 || p.Interval <= 0 || p.FastThreshold <= 0 || p.MixedThreshold <= 0 || p.MaxProbes <= 0 || p.Capacity <= 0 {
			return fmt.Errorf("breaker policy must explicitly contain positive values")
		}
		return breakerpolicy.ValidatePolicy(p)
	}
	if err := check(d.Breaker.Defaults); err != nil {
		return err
	}
	for s, b := range d.Breaker.Sites {
		if !known[s] {
			return fmt.Errorf("unknown site %q", s)
		}
		if err := check(b); err != nil {
			return fmt.Errorf("site %s: %w", s, err)
		}
	}
	return nil
}
func (d Document) Clone() Document {
	c := d
	c.Breaker.Sites = make(map[string]Breaker, len(d.Breaker.Sites))
	for k, v := range d.Breaker.Sites {
		c.Breaker.Sites[k] = v
	}
	return c
}
