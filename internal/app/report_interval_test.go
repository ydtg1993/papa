package app

import (
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

// 定时报告与退出报告共用日报产物：调用一次就要有 4 个文件，目录按本地日期。
func TestGeneratePeriodicReportWritesDailyFiles(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	eventTS := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	writeEvents(t, filepath.Join(logDir, "events.jsonl"), []map[string]any{
		{"v": 1, "seq": 1, "ts": eventTS.Format(time.RFC3339Nano), "run_id": "r-auto", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": eventTS.Add(time.Minute).Format(time.RFC3339Nano), "run_id": "r-auto", "type": "task_finished",
			"site": "s", "stage": "catalog", "task_id": 1, "outcome": "success"},
	})

	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.Interval = time.Hour // 只用于通过"已开启"判断
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")
	cfg.Obs.Report.Retention = 168 * time.Hour
	cfg.Obs.Report.Timeout = 5 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}

	a.generatePeriodicReport()

	reportDir := filepath.Join(cfg.Obs.Report.Dir, report.DayName(eventTS))
	for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
		path := filepath.Join(reportDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("缺少定时报告产物 %s：%v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("产物 %s 为空", name)
		}
	}
}

// 定时器真的会触发：interval 很短时 start 后应写出日报；stop 可重复调用且不再写。
func TestStartReportLoopWritesAndStops(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	eventTS := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	writeEvents(t, filepath.Join(logDir, "events.jsonl"), []map[string]any{
		{"v": 1, "seq": 1, "ts": eventTS.Format(time.RFC3339Nano), "run_id": "r-loop", "type": "run_started"},
	})

	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.Interval = 20 * time.Millisecond
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")
	cfg.Obs.Report.Retention = 168 * time.Hour
	cfg.Obs.Report.Timeout = 5 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}

	a.startReportLoop()
	reportPath := filepath.Join(cfg.Obs.Report.Dir, report.DayName(eventTS), "report.md")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(reportPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			a.stopReportLoop()
			t.Fatalf("定时报告未在 3s 内生成：%v", reportPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.stopReportLoop()
	a.stopReportLoop() // 幂等

	before, err := os.Stat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // 超过几个 interval：已停的 loop 不该再刷新
	after, err := os.Stat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("stopReportLoop 之后仍在写报告：before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

// 没开事件流 / interval 为 0：不启动、不建目录。
func TestStartReportLoopDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Log.Dir = dir
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}
	a.startReportLoop()
	a.stopReportLoop()

	if _, err := os.Stat(cfg.Obs.Report.Dir); !os.IsNotExist(err) {
		t.Fatalf("关闭时不应创建报告目录：%v", err)
	}
}
