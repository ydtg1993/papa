package config

import (
	"strings"
	"testing"
	"time"
)

func TestGovernanceQueuesDefaultAndZeroIntervals(t *testing.T) {
	for _, extra := range []string{"", "error_queue:\n  interval: 0\nrepeat_queue:\n  interval: 0\n"} {
		cfg := loadString(t, baseConfig+extra)
		if cfg.ErrorQueue.EffectiveInterval() != 4*time.Hour || cfg.RepeatQueue.EffectiveInterval() != 12*time.Hour {
			t.Fatalf("mandatory queue defaults missing: %+v / %+v", cfg.ErrorQueue, cfg.RepeatQueue)
		}
	}
}

func TestGovernanceQueuesRejectRemovedSwitchesAndNegativeValues(t *testing.T) {
	for _, queue := range []string{"error_queue", "repeat_queue"} {
		for _, field := range []string{"enabled: false", "enabled: true", "interval: -1s", "worker_count: -1", "batch_size: -1"} {
			key := queue + "." + strings.Split(field, ":")[0]
			if msg := panicOn(t, baseConfig+queue+":\n  "+field+"\n"); !strings.Contains(msg, key) {
				t.Fatalf("expected %s: %s", key, msg)
			}
		}
	}
}
