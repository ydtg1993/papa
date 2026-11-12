package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBreakerPolicyYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := baseConfig + "crawler:\n  breaker:\n    enabled: true\n    threshold: 20\n    policy:\n      short_window: 12s\n      fast_threshold: 4\n      max_recovery: 6m\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Crawler.Breaker.Policy
	if got.ShortWindow != 12*time.Second || got.FastThreshold != 4 || got.MaxRecovery != 6*time.Minute {
		t.Fatalf("YAML policy lost: %+v", got)
	}
}
