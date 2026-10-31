package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/obs/report"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
)

func newReportTestApp(t *testing.T, now time.Time, events []map[string]any) (*App, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEvents(t, filepath.Join(logDir, "events.jsonl"), events)
	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.Interval = time.Hour
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")
	cfg.Obs.Report.Retention = 168 * time.Hour
	cfg.Obs.Report.Timeout = 5 * time.Second
	cfg.Obs.Report.Mode = "auto"
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}, cfg
}

func readReportJSON(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("读 report.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 report.json: %v", err)
	}
	return m
}

func TestPeriodicReportIsSnapshotAndOnlyToday(t *testing.T) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	yesterdayStart := todayStart.AddDate(0, 0, -1)
	evs := []map[string]any{
		{"v": 1, "seq": 1, "ts": yesterdayStart.Format(time.RFC3339Nano), "run_id": "r-old", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": todayStart.Format(time.RFC3339Nano), "run_id": "r-new", "type": "run_started"},
	}
	a, cfg := newReportTestApp(t, now, evs)

	a.generatePeriodicReport()

	todayDir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(todayStart))
	m := readReportJSON(t, todayDir)
	if m["mode"] != "snapshot" {
		t.Fatalf("定时报告 mode = %v, want snapshot", m["mode"])
	}
	if _, err := os.Stat(filepath.Join(cfg.Obs.Report.Dir, report.DayName(yesterdayStart))); !os.IsNotExist(err) {
		t.Fatalf("snapshot 模式不该写历史日目录（yesterday 应不存在）：%v", err)
	}
	run := m["run"].(map[string]any)
	from, _ := time.Parse(time.RFC3339Nano, run["from"].(string))
	if from.Hour() != 0 || from.Minute() != 0 {
		t.Fatalf("snapshot 窗口应从当天 00:00 起，实得 %v", from)
	}
	// 上界必须钉在生成时刻：否则「当天」窗口会一路延到 23:59:59，snapshot 语义失真。
	to, err := time.Parse(time.RFC3339Nano, run["to"].(string))
	if err != nil {
		t.Fatalf("解析 run.to: %v", err)
	}
	if d := time.Since(to); d < 0 || d > time.Minute {
		t.Fatalf("snapshot 窗口上界应是生成时刻（距 time.Now 不超过 1 分钟），实得 to=%v（相差 %v）", to, d)
	}
}

func TestExplicitExitAcceptanceWritesAllDays(t *testing.T) {
	now := time.Now()
	todayNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	yesterdayNoon := todayNoon.AddDate(0, 0, -1)
	evs := []map[string]any{
		{"v": 1, "seq": 1, "ts": yesterdayNoon.Format(time.RFC3339Nano), "run_id": "r-old", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": todayNoon.Format(time.RFC3339Nano), "run_id": "r-new", "type": "run_started"},
	}
	a, cfg := newReportTestApp(t, now, evs)
	a.Config.Obs.Report.OnExit = true
	a.Config.Obs.Report.Mode = "acceptance"

	a.generateExitReport()

	for _, day := range []time.Time{yesterdayNoon, todayNoon} {
		dir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(day))
		if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
			t.Fatalf("exit(acceptance) 应写 %s：%v", report.DayName(day), err)
		}
		if got := readReportJSON(t, dir)["mode"]; got != "acceptance" {
			t.Fatalf("%s mode = %v, want acceptance", report.DayName(day), got)
		}
	}
}

func TestObsEngineStateNilEngine(t *testing.T) {
	a := &App{Config: &config.Config{}}
	st := a.obsEngineState()
	if st.Source != "none" {
		t.Fatalf("无引擎时 Source = %q, want none", st.Source)
	}
}

func TestExitAutoReportIsSnapshotAndOnlyToday(t *testing.T) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	a, cfg := newReportTestApp(t, now, []map[string]any{
		{"v": 1, "seq": 1, "ts": start.AddDate(0, 0, -1).Format(time.RFC3339Nano), "run_id": "old", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": start.Format(time.RFC3339Nano), "run_id": "today", "type": "run_started"},
	})
	cfg.Obs.Report.OnExit = true
	a.generateExitReport()
	if got := readReportJSON(t, filepath.Join(cfg.Obs.Report.Dir, report.DayName(start)))["mode"]; got != "snapshot" {
		t.Fatalf("auto exit mode=%v", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.Obs.Report.Dir, report.DayName(start.AddDate(0, 0, -1)))); !os.IsNotExist(err) {
		t.Fatalf("auto exit must not rewrite history: %v", err)
	}
}
