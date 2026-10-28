package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 启动恢复的批间停顿：不写 = 默认 30m；写了 = 用写的值。
// 这是"一次恢复别把队列灌爆"的节拍开关，默认值必须有测试钉住 —— 否则谁改坏了默认，
// 线上重启会在启动瞬间重新灌入全部积压。
func TestRecoverQueueBatchPauseDefaultAndOverride(t *testing.T) {
	base := "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n" +
		"recover_queue:\n  enabled: true\n  worker_count: 1\n  batch_size: 200\n"

	cases := []struct {
		name    string
		extra   string
		want    time.Duration
		wantErr bool
	}{
		{"不写 batch_pause → 默认 30m", "", 30 * time.Minute, false},
		{"显式 5s", "  batch_pause: \"5s\"\n", 5 * time.Second, false},
		{"负数归一后仍走默认 30m", "  batch_pause: \"-1s\"\n", 30 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte(base+tc.extra), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.RecoverQueue.BatchPause != tc.want {
				t.Fatalf("batch_pause = %v, want %v", cfg.RecoverQueue.BatchPause, tc.want)
			}
		})
	}
}
