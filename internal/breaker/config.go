// Package breaker classifies bounded terminal task outcomes and decides protection.
// Pause ownership and recovery belong to engine; this package owns no worker gate.
package breaker

import (
	"fmt"
	"time"
)

const defaultWindow = 5 * time.Minute

type Config struct {
	Policy    Policy
	Enabled   bool
	Window    time.Duration
	Threshold int
}

func EffectiveConfig(cfg Config) Config {
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	cfg.Policy = EffectivePolicy(cfg.Policy)
	return cfg
}

func Validate(cfg Config) error {
	if cfg.Enabled && cfg.Threshold <= 0 {
		return fmt.Errorf("crawler.breaker.enabled=true 时 threshold 必须 > 0，实得 %d", cfg.Threshold)
	}
	return ValidatePolicy(cfg.Policy)
}
