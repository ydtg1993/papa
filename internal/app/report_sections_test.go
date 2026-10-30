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
	} else if !strings.Contains(v, "m") {
		t.Fatalf("inputs.thresholds.stuck_after 应是可读时长（如 10m0s），实得 %q", v)
	}
}

// EngineState.Recovering 必须逐日报源填：同一次 exit 报告里，有 recover 重投的那天为 true，
// 没有的那天不能被污染 —— 进程级 engine 快照是共享的，只有逐源 fill 才正确。
func TestDailyReportFillsRecoveringPerDaySource(t *testing.T) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	yesterdayStart := todayStart.AddDate(0, 0, -1)
	evs := []map[string]any{
		{"v": 1, "seq": 1, "ts": yesterdayStart.Add(time.Minute).Format(time.RFC3339Nano),
			"run_id": "r-old", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": todayStart.Format(time.RFC3339Nano), "run_id": "r-new", "type": "run_started"},
		{"v": 1, "seq": 3, "ts": todayStart.Add(time.Minute).Format(time.RFC3339Nano), "run_id": "r-new",
			"type": "task_requeued", "site": "s", "stage": "detail", "task_id": 9,
			"queue": "recover_queue", "source": "auto"},
	}
	a, cfg := newReportTestApp(t, now, evs)
	a.Config.Obs.Report.OnExit = true

	a.generateExitReport()

	for _, tc := range []struct {
		day  time.Time
		want bool
		why  string
	}{
		{todayStart, true, "当天有 recover_queue 重投，Recovering 应为 true"},
		{yesterdayStart, false, "前一天没有 recover 事件，不该被今天的恢复状态污染"},
	} {
		dir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(tc.day))
		m := readReportJSON(t, dir)
		eng, _ := m["engine"].(map[string]any)
		if eng == nil {
			t.Fatalf("%s 缺 engine 段：%v", report.DayName(tc.day), m)
		}
		got, _ := eng["recovering"].(bool)
		if got != tc.want {
			t.Fatalf("%s engine.recovering = %v, want %v（%s；engine=%v）",
				report.DayName(tc.day), got, tc.want, tc.why, eng)
		}
	}
}
