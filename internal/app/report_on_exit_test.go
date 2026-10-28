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
	"github.com/ydtg1993/papa/v3/pkg/loggers"
)

// 进程退出自动出报告（obs.report.on_exit）：事件流开启时，退出流程要写出
// report.md / report.json / ai-brief.md / ai-brief.json 四个文件。
func TestGenerateExitReportWritesFiles(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lines := []map[string]any{
		{"v": 1, "seq": 1, "ts": now.Add(-time.Minute).Format(time.RFC3339Nano), "run_id": "r-exit", "type": "run_started"},
		{"v": 1, "seq": 2, "ts": now.Add(-30 * time.Second).Format(time.RFC3339Nano), "run_id": "r-exit", "type": "task_finished",
			"site": "s", "stage": "catalog", "task_id": 1, "outcome": "success", "duration_ms": 120},
		{"v": 1, "seq": 3, "ts": now.Format(time.RFC3339Nano), "run_id": "r-exit", "type": "run_stopped", "outcome": "drained"},
	}
	f, err := os.Create(filepath.Join(logDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Log.Dir = logDir
	cfg.Obs.Events.Enabled = true
	cfg.Obs.Report.OnExit = true
	cfg.Obs.Report.Dir = filepath.Join(dir, "reports")
	cfg.Obs.Report.Timeout = 5 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	a := &App{Config: cfg, Logger: &loggers.LoggerSet{Sys: logger}}

	a.generateExitReport()

	reportDir := filepath.Join(cfg.Obs.Report.Dir, "r-exit")
	for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
		path := filepath.Join(reportDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("缺少产物 %s：%v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("产物 %s 为空", name)
		}
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
