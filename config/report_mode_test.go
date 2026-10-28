package config

import (
	"strings"
	"testing"
	"time"
)

// obs.report.mode / thresholds.event_max_gap：两个可选键的三态。
//
//	没写 → mode=auto、event_max_gap=10m（生效值在 Validate 里归一，消费方直接读）；
//	写了合法 → 原样保留（mode 大小写与首尾空白在归一阶段吃掉）；
//	写了越界 → 启动 panic，并点名配置键（不然用户不知道去哪改）。
func TestReportModeDefaultAndOverride(t *testing.T) {
	cfg := loadString(t, baseConfig)
	if got := cfg.Obs.Report.Mode; got != "auto" {
		t.Fatalf("obs.report.mode 默认 = %q, want auto", got)
	}
	if got := cfg.Obs.Report.Thresholds.EventMaxGap; got != 10*time.Minute {
		t.Fatalf("event_max_gap 默认 = %v, want 10m", got)
	}
	cfg = loadString(t, baseConfig+`obs:
  report:
    mode: snapshot
    thresholds:
      event_max_gap: 2m
`)
	if got := cfg.Obs.Report.Mode; got != "snapshot" {
		t.Fatalf("mode = %q, want snapshot", got)
	}
	if got := cfg.Obs.Report.Thresholds.EventMaxGap; got != 2*time.Minute {
		t.Fatalf("event_max_gap = %v, want 2m", got)
	}
}

func TestReportModeRejectsUnknownValue(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("非法 mode 必须 panic（配置校验层语义）")
		}
	}()
	loadString(t, baseConfig+`obs:
  report:
    mode: weekly
`)
}

func TestEventMaxGapRejectsNegative(t *testing.T) {
	defer func() {
		r := recover()
		err, ok := r.(error)
		if !ok || !strings.Contains(err.Error(), "event_max_gap") {
			t.Fatalf("负数 event_max_gap 必须 panic 且点名键，实得 %v", r)
		}
	}()
	loadString(t, baseConfig+`obs:
  report:
    thresholds:
      event_max_gap: -1s
`)
}
