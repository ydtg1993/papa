package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs/report"
)

// TestPeriodicReportIncludesEngineAndBacklogSections 验证定时报告真的注册了
// 「引擎状态」「积压与老化」两段，并且 report.json 带上了自证信息。
//
// 事件时间取当天 0 点而不是计划里的 12:00：snapshot 窗口是 [当天 0 点, 生成时刻]，
// 凌晨跑测试时 12:00 还在未来，会被窗口上界裁掉、整份报告因「没有事件」跳过。
func TestPeriodicReportIncludesEngineAndBacklogSections(t *testing.T) {
	now := time.Now()
	ts := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	a, cfg := newReportTestApp(t, now, []map[string]any{
		{"v": 1, "seq": 1, "ts": ts.Format(time.RFC3339Nano), "run_id": "r-x", "type": "run_started"},
	})

	a.generatePeriodicReport()

	dir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(ts))
	raw, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"引擎状态", "积压与老化"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("report.md 缺段 %q：\n%s", want, raw)
		}
	}

	m := readReportJSON(t, dir)
	inputs, _ := m["inputs"].(map[string]any)
	if inputs == nil {
		t.Fatalf("report.json 缺 inputs：%v", m)
	}
	if v, _ := inputs["papa_version"].(string); v == "" {
		t.Fatalf("inputs.papa_version 为空：%v", inputs)
	}
	thresholds, _ := inputs["thresholds"].(map[string]any)
	if thresholds == nil {
		t.Fatalf("inputs.thresholds 缺失：%v", inputs)
	}
	if v, _ := thresholds["stuck_after"].(string); v == "" {
		t.Fatalf("inputs.thresholds.stuck_after 为空：%v", thresholds)
	}
}
