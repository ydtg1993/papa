package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs/report"
)

// writeReportTestEvents 造一份跨两天的事件流（昨天 + 今天各一个 run）。
func writeReportTestEvents(t *testing.T, path string, now time.Time) {
	t.Helper()
	lines := []string{
		fmt.Sprintf(`{"v":1,"seq":1,"ts":%q,"run_id":"r-y","type":"run_started"}`, now.AddDate(0, 0, -1).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":2,"ts":%q,"run_id":"r-y","type":"task_finished","site":"s","stage":"catalog","task_id":1,"outcome":"success","duration_ms":10}`, now.AddDate(0, 0, -1).Add(time.Minute).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":1,"ts":%q,"run_id":"r-t","type":"run_started"}`, now.Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":2,"ts":%q,"run_id":"r-t","type":"task_finished","site":"s","stage":"catalog","task_id":2,"outcome":"success","duration_ms":20}`, now.Add(time.Minute).Format(time.RFC3339Nano)),
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写事件文件: %v", err)
	}
}

// writeReportTestConfig 写一份最小可用配置：log.dir 指到临时日志目录，报告目录与保留期显式给出。
func writeReportTestConfig(t *testing.T, dir, logDir, reportsDir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`log:
  dir: %q
db:
  max_idle_conns: 10
  max_open_conns: 100
obs:
  events:
    enabled: true
  report:
    dir: %q
    retention: "168h"
`, filepath.ToSlash(logDir), filepath.ToSlash(reportsDir))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	return cfgPath
}

// 默认生成：按本地日期拆日报（昨天/今天各一份），生成后清理超过保留期的旧日报目录。
func TestReportGenerateSplitsDailyAndPrunes(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportsDir := filepath.Join(dir, "reports")
	now := time.Now()
	writeReportTestEvents(t, filepath.Join(logDir, "events.jsonl"), now)
	cfgPath := writeReportTestConfig(t, dir, logDir, reportsDir)

	// 超过保留期的旧日报目录（10 天前）→ 跑完应被清理。
	oldDayDir := filepath.Join(reportsDir, report.DayName(now.AddDate(0, 0, -10)))
	if err := os.MkdirAll(oldDayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDayDir, "report.md"), []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runReportCmd([]string{"generate", "-c", cfgPath, "--no-logs"}); err != nil {
		t.Fatalf("runReportCmd: %v", err)
	}

	for _, day := range []string{report.DayName(now.AddDate(0, 0, -1)), report.DayName(now)} {
		for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
			if _, err := os.Stat(filepath.Join(reportsDir, day, name)); err != nil {
				t.Fatalf("缺少日报 %s/%s：%v", day, name, err)
			}
		}
	}
	if _, err := os.Stat(oldDayDir); !os.IsNotExist(err) {
		t.Errorf("超过保留期的日报目录应被清理（err=%v）", err)
	}
}

// 显式 --run：只出单 run 报告（目录名 = run_id），不按天拆分。
func TestReportGenerateRunFlagKeepsSingleReport(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportsDir := filepath.Join(dir, "reports")
	now := time.Now()
	writeReportTestEvents(t, filepath.Join(logDir, "events.jsonl"), now)
	cfgPath := writeReportTestConfig(t, dir, logDir, reportsDir)

	if err := runReportCmd([]string{"generate", "-c", cfgPath, "--run", "r-t", "--no-logs"}); err != nil {
		t.Fatalf("runReportCmd: %v", err)
	}
	for _, name := range []string{"report.md", "report.json"} {
		if _, err := os.Stat(filepath.Join(reportsDir, "r-t", name)); err != nil {
			t.Fatalf("缺少单 run 报告 r-t/%s：%v", name, err)
		}
	}
	// --run 不走按天拆分：不该出现日期目录。
	if _, err := os.Stat(filepath.Join(reportsDir, report.DayName(now))); !os.IsNotExist(err) {
		t.Errorf("--run 不应生成日报目录（err=%v）", err)
	}
}
