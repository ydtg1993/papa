package app

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
)

func TestBreakerSpecPolicyInheritanceAndReplacement(t *testing.T) {
	def := config.BreakerConfig{Enabled: true, Threshold: 20, Policy: config.BreakerPolicy{FastThreshold: 7, ShortWindow: 12 * time.Second}}
	got, err := (BreakerSpec{Enabled: true}).toConfig(def)
	if err != nil || got.Policy.FastThreshold != 7 || got.Policy.ShortWindow != 12*time.Second {
		t.Fatalf("inherit: %+v %v", got, err)
	}
	p := config.BreakerPolicy{FastThreshold: 3}
	got, err = (BreakerSpec{Enabled: true, Policy: &p}).toConfig(def)
	if err != nil || got.Policy.FastThreshold != 3 || got.Policy.ShortWindow != 0 {
		t.Fatalf("replace: %+v %v", got, err)
	}
	p.FastThreshold = -1
	if _, err := (BreakerSpec{Enabled: true, Policy: &p}).toConfig(def); err == nil {
		t.Fatal("invalid site policy accepted")
	}
}
