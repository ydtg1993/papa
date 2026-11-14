package config

import (
	"strings"
	"testing"
	"time"
)

func TestQueueDefaultsAndRemovedKeys(t *testing.T) {
	for _, body := range []string{"", "error_queue:\n  interval: 0\nrepeat_queue:\n  interval: 0\n"} {
		cfg := loadString(t, baseConfig+body)
		if cfg.ErrorQueue.Interval != 4*time.Hour || cfg.RepeatQueue.Interval != 12*time.Hour || cfg.ErrorQueue.WorkerCount != 1 || cfg.RepeatQueue.WorkerCount != 1 {
			t.Fatalf("unexpected defaults: %+v %+v", cfg.ErrorQueue, cfg.RepeatQueue)
		}
	}
	for _, tc := range []struct{ section, key, value string }{
		{"crawler", "queue_watermark", "0.75"}, {"crawler", "drain_interval", "2s"},
		{"recover_queue", "enabled", "false"}, {"recover_queue", "worker_count", "1"}, {"recover_queue", "batch_pause", "1ms"},
		{"error_queue", "enabled", "false"}, {"repeat_queue", "enabled", "false"},
	} {
		t.Run(tc.section+"."+tc.key, func(t *testing.T) {
			msg := panicOn(t, baseConfig+tc.section+":\n  "+tc.key+": "+tc.value+"\n")
			if !strings.Contains(msg, tc.key) {
				t.Fatalf("removed key accepted or unclear error: %s", msg)
			}
		})
	}
	for _, section := range []string{"error_queue", "repeat_queue"} {
		msg := panicOn(t, baseConfig+section+":\n  interval: -1s\n")
		if !strings.Contains(msg, section+".interval") {
			t.Fatal(msg)
		}
	}
}
