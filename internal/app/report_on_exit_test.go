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

// writeEvents 把一批 (ts, run_id, type) 写成 JSONL 事件文件。
func writeEvents(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}

// 进程退出自动出报告（obs.report.on_exit）：产物按本地日期拆到 <dir>/<2006-01-02>/，
// 每份都有 report.md / report.json / ai-brief.md / ai-brief.json 四个文件。
func TestGenerateExitReportWritesDailyFiles(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 固定在"今天中午"造事件，避免测试跑在午夜边界时把事件分到两天。
	now := time.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	writeEvents(t, filepath.Join(logDir, "events.jsonl"), []map[string]any{
		{"v": 1, "seq": 1, "ts": noon.Format(time.RFC3339Nano), "run_id": "r-exit", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": noon.Add(30 * time.Second).Format(time.RFC3339Nano), "run_id": "r-exit", "type": "task_finished",
			"site": "s", "stage": "catalog", "task_id": 1, "outcome": "success", "duration_ms": 120},
		{"v": 1, "seq": 3, "ts": noon.Add(time.Minute).Format(time.RFC3339Nano), "run_id": "r-exit", "type": "run_stopped", "outcome": "drained"},
	})

	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.OnExit = true
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")
	cfg.Obs.Report.Retention = 168 * time.Hour
	cfg.Obs.Report.Timeout = 5 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}

	a.generateExitReport()

	reportDir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(noon))
	for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
		path := filepath.Join(reportDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("缺少产物 %s：%v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("产物 %s 为空", path)
		}
	}
}

// 事件横跨两天 → 每天各一份日报（目录名是各自的本地日期）；写完后按保留期清理过期目录：
// 老日报（名字里的日期太老）与老命名的 window-* 目录都会删，没有 report.md 的目录与今天的报告不碰。
func TestGenerateExitReportSplitsDaysAndPrunes(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	writeEvents(t, filepath.Join(logDir, "events.jsonl"), []map[string]any{
		{"v": 1, "seq": 1, "ts": yesterday.Format(time.RFC3339Nano), "run_id": "r-y", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": now.Format(time.RFC3339Nano), "run_id": "r-t", "type": "run_started"},
	})

	reportsDir := filepath.Join(dir, "reports")
	// 超过 7 天的日报目录 + 旧命名的 window-* 目录（mtime 也老）→ 应被清理。
	oldDay := report.DayName(now.AddDate(0, 0, -10))
	oldDayDir := filepath.Join(reportsDir, oldDay)
	if err := os.MkdirAll(oldDayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDayDir, "report.md"), []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(reportsDir, "window-20260101-000000")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "report.md"), []byte("# legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.AddDate(0, 0, -30)
	if err := os.Chtimes(legacyDir, old, old); err != nil {
		t.Fatal(err)
	}
	// 无关目录（没有 report.md）不该被清理。
	notesDir := filepath.Join(reportsDir, "notes")
	if err := os.MkdirAll(notesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(notesDir, old, old); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.OnExit = true
	cfg.Obs.Report.Dir = reportsDir
	cfg.Obs.Report.Retention = 168 * time.Hour
	cfg.Obs.Report.Timeout = 10 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}

	a.generateExitReport()

	for _, day := range []string{report.DayName(yesterday), report.DayName(now)} {
		for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
			if _, err := os.Stat(filepath.Join(reportsDir, day, name)); err != nil {
				t.Fatalf("缺少 %s/%s：%v", day, name, err)
			}
		}
	}
	for _, gone := range []string{oldDay, "window-20260101-000000"} {
		if _, err := os.Stat(filepath.Join(reportsDir, gone)); !os.IsNotExist(err) {
			t.Errorf("过期报告目录 %s 应被清理（err=%v）", gone, err)
		}
	}
	if _, err := os.Stat(notesDir); err != nil {
		t.Errorf("没有 report.md 的目录不该被清理：%v", err)
	}
}

// 事件流没开 / on_exit 没开时，退出报告必须是 no-op（不 panic、不写目录）。
func TestGenerateExitReportSkipsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Log.Dir = dir
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}
	a.generateExitReport()

	if _, err := os.Stat(cfg.Obs.Report.Dir); !os.IsNotExist(err) {
		t.Fatalf("关闭时不应创建报告目录：%v", err)
	}
}
