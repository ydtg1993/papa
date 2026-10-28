package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// obs.report.interval：不写 = 0（不自动生成）；写了按 duration 解析。
func TestObsReportIntervalDefaultAndOverride(t *testing.T) {
	base := "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n"
	cases := []struct {
		name  string
		extra string
		want  time.Duration
	}{
		{"不写 → 0（关闭）", "", 0},
		{"显式 30m", "obs:\n  report:\n    interval: \"30m\"\n", 30 * time.Minute},
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
			if cfg.Obs.Report.Interval != tc.want {
				t.Fatalf("interval = %v, want %v", cfg.Obs.Report.Interval, tc.want)
			}
		})
	}
}

// 负数间隔没有语义，必须在启动时炸掉（否则 time.NewTicker panic 会被当成运行时崩溃）。
func TestObsReportIntervalRejectsNegative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n" +
		"obs:\n  report:\n    interval: \"-1s\"\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("负数 interval 应当启动失败")
		}
	}()
	_, _ = Load(p)
}
