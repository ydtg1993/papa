package config

import (
	"strings"
	"testing"
)

func TestRemovedQueueOptionsAreRejected(t *testing.T) {
	for _, tc := range []struct{ section, key, value string }{
		{"crawler", "queue_watermark", "0.75"},
		{"crawler", "drain_interval", "2s"},
		{"recover_queue", "enabled", "true"},
		{"recover_queue", "worker_count", "1"},
		{"recover_queue", "batch_pause", "30m"},
	} {
		t.Run(tc.section+"."+tc.key, func(t *testing.T) {
			msg := panicOn(t, baseConfig+tc.section+":\n  "+tc.key+": "+tc.value+"\n")
			if !strings.Contains(msg, tc.key) {
				t.Fatalf("removed key not identified: %s", msg)
			}
		})
	}
}

func TestRecoveryBatchSizeOnlyConfiguration(t *testing.T) {
	for _, size := range []string{"0", "100", "2000"} {
		loadString(t, baseConfig+"recover_queue:\n  batch_size: "+size+"\n")
	}
	if msg := panicOn(t, baseConfig+"recover_queue:\n  batch_size: -1\n"); !strings.Contains(msg, "batch_size") {
		t.Fatalf("negative batch size accepted: %s", msg)
	}
}
