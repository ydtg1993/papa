package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/report"
)

// writeReportTestEvents 造一份跨两天的事件流（昨天 + 今天各一个 run）。
func writeReportTestEvents(t *testing.T, path string, now time.Time) {
	t.Helper()
	lines := []string{
		fmt.Sprintf(`{"v":1,"seq":1,"ts":%q,"run_id":"r-y","type":"run_started"}`, now.AddDate(0, 0, -1).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":2,"ts":%q,"run_id":"r-y","type":"task_finished","site":"s","stage":"catalog","task_id":1,"outcome":"success","duration_ms":10}`, now.AddDate(0, 0, -1).Add(time.Minute).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":3,"ts":%q,"run_id":"r-y","type":"stage_snapshot","stages":{"s:catalog":{"workers":1}}}`, now.AddDate(0, 0, -1).Add(time.Minute).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":1,"ts":%q,"run_id":"r-t","type":"run_started"}`, now.Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":2,"ts":%q,"run_id":"r-t","type":"task_finished","site":"s","stage":"catalog","task_id":2,"outcome":"success","duration_ms":20}`, now.Add(time.Minute).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"v":1,"seq":3,"ts":%q,"run_id":"r-t","type":"stage_snapshot","stages":{}}`, now.Add(time.Minute).Format(time.RFC3339Nano)),
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
  dsn: "invalid-dsn-for-offline-report-tests"
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
		raw, err := os.ReadFile(filepath.Join(reportsDir, day, "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rep obs.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Mode != obs.ModeSnapshot {
			t.Fatalf("默认日报 mode=%q, want snapshot", rep.Mode)
		}
		wantPools := 0
		if day == report.DayName(now.AddDate(0, 0, -1)) {
			wantPools = 1
		}
		if rep.Engine.ActivePools != wantPools {
			t.Fatalf("%s 活跃池=%d, want %d（不能跨天缓存状态）", day, rep.Engine.ActivePools, wantPools)
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

func TestReportGenerateExplicitHistoryRetained(t *testing.T) {
	for _, selector := range []string{"window", "since", "until", "run", "run-date"} {
		t.Run(selector, func(t *testing.T) {
			dir := t.TempDir()
			logDir, out := filepath.Join(dir, "logs"), filepath.Join(dir, "reports")
			if err := os.Mkdir(logDir, 0700); err != nil {
				t.Fatal(err)
			}
			old := time.Now().AddDate(0, 0, -30)
			old = time.Date(old.Year(), old.Month(), old.Day(), 12, 0, 0, 0, time.Local)
			events := filepath.Join(logDir, "events.jsonl")
			writeReportTestEvents(t, events, old)
			raw, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			runID := ".."
			if selector == "run-date" {
				// A dated run directory expires by its name even when freshly written.
				// This detects accidentally retaining only the time-window exemption.
				runID = report.DayName(old)
			}
			if err := os.WriteFile(events, []byte(strings.ReplaceAll(string(raw), "r-t", runID)), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := writeReportTestConfig(t, dir, logDir, out)
			args := []string{"generate", "-c", cfg, "--no-logs"}
			label := report.DayName(old)
			switch selector {
			case "window":
				args = append(args, "--since", old.Add(-time.Hour).Format(time.RFC3339), "--until", old.Add(time.Hour).Format(time.RFC3339))
			case "since":
				args = append(args, "--since", old.Add(-time.Hour).Format(time.RFC3339))
			case "until":
				args = append(args, "--until", old.Add(time.Hour).Format(time.RFC3339))
			default:
				args = append(args, "--run", runID)
				label = report.DirName(obs.RunMeta{RunIDs: []string{runID}})
			}
			if err := runReportCmd(args); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
				if _, err := os.Stat(filepath.Join(out, label, name)); err != nil {
					t.Fatalf("missing output %s: %v", name, err)
				}
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("output escaped: %s (%v)", name, err)
				}
			}
		})
	}
}
func TestReportOutputDirRejectsTraversal(t *testing.T) {
	for _, label := range []string{"", ".", "..", "../escape", `..\escape`, "/absolute", "C:stream", "trailing."} {
		if _, err := reportOutputDir(t.TempDir(), label); err == nil {
			t.Errorf("accepted %q", label)
		}
	}
}
