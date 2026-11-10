package config

import (
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"time"
)

// RuntimeConfig contains explicit overrides only. A nil field inherits its
// declaration/default; a pointer to false or zero overrides it deliberately.
type RuntimeConfig struct {
	Defaults    governance.PolicyPatch            `json:"defaults,omitempty" mapstructure:"defaults"`
	Sites       map[string]governance.PolicyPatch `json:"sites,omitempty" mapstructure:"sites"`
	Resources   governance.ResourcePatch          `json:"resources,omitempty" mapstructure:"resources"`
	ProxyHealth ProxyHealthPatch                  `json:"proxy_health,omitempty" mapstructure:"proxy_health"`
}

type ProxyHealthPatch struct {
	Enabled           *bool          `json:"enabled,omitempty" mapstructure:"enabled"`
	FailureThreshold  *int           `json:"failure_threshold,omitempty" mapstructure:"failure_threshold"`
	InitialCooldown   *time.Duration `json:"initial_cooldown,omitempty" mapstructure:"initial_cooldown"`
	MaxCooldown       *time.Duration `json:"max_cooldown,omitempty" mapstructure:"max_cooldown"`
	MaxProbes         *int           `json:"max_probes,omitempty" mapstructure:"max_probes"`
	RecoverySuccesses *int           `json:"recovery_successes,omitempty" mapstructure:"recovery_successes"`
}

func (p ProxyHealthPatch) Apply(v proxy.HealthConfig) proxy.HealthConfig {
	if p.Enabled != nil {
		v.Enabled = *p.Enabled
	}
	if p.FailureThreshold != nil {
		v.FailureThreshold = *p.FailureThreshold
	}
	if p.InitialCooldown != nil {
		v.InitialCooldown = *p.InitialCooldown
	}
	if p.MaxCooldown != nil {
		v.MaxCooldown = *p.MaxCooldown
	}
	if p.MaxProbes != nil {
		v.MaxProbes = *p.MaxProbes
	}
	if p.RecoverySuccesses != nil {
		v.RecoverySuccesses = *p.RecoverySuccesses
	}
	return v
}

func (p RuntimeConfig) Validate() error {
	c := governance.DefaultConfig()
	c.Defaults = governance.MergePolicy(c.Defaults, p.Defaults)
	c.Sites = p.Sites
	c.Resources = governance.MergeResources(c.Resources, p.Resources)
	if err := c.Validate(); err != nil {
		return err
	}
	return p.ProxyHealth.Apply(proxy.DefaultHealthConfig()).Validate()
}
