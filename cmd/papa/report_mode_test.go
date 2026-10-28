package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/obs/report"
)

func TestParseReportArgsSnapshot(t *testing.T) {
	opts, err := parseReportArgs([]string{"--snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.snapshot {
		t.Fatal("--snapshot 没解析到")
	}
}

func TestSnapshotWindowRejectsCrossDay(t *testing.T) {
	loc := time.Local
	from := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	to := time.Date(2026, 10, 29, 1, 0, 0, 0, loc)
	if _, _, err := snapshotWindow(from, to, loc); err == nil {
		t.Fatal("跨本地日应报错")
	}
	day, end, err := snapshotWindow(from, from.Add(2*time.Hour), loc)
	if err != nil {
		t.Fatal(err)
	}
	if day.Hour() != 0 || !end.Equal(from.Add(2*time.Hour)) {
		t.Fatalf("单日窗口 = %v ~ %v", day, end)
	}
}

func TestVerdictGate(t *testing.T) {
	if err := verdictGate(obs.VerdictFail, true); err == nil {
		t.Fatal("fail + --fail-on-verdict 应非零")
	}
	if err := verdictGate(obs.VerdictUnknown, true); err == nil {
		t.Fatal("unknown + --fail-on-verdict 应非零（门禁不能把未判定当通过）")
	}
	if err := verdictGate(obs.VerdictUnknown, false); err != nil {
		t.Fatalf("没开 --fail-on-verdict 时 unknown 不该报错：%v", err)
	}
	if err := verdictGate(obs.VerdictWarn, true); err != nil {
		t.Fatalf("warn 不在门禁内：%v", err)
	}
}

// 默认 generate（按天拆分）走的是 CLI 的段注册与 Inputs：读今天那份 report.json，
// 确认版本与 event_max_gap 生效值真的写进了自证信息（不是只有 App 报告才有）。
func TestReportGenerateWritesInputs(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportsDir := filepath.Join(dir, "reports")
	now := time.Now()
	writeReportTestEvents(t, filepath.Join(logDir, "events.jsonl"), now)
	cfgPath := writeReportTestConfig(t, dir, logDir, reportsDir)

	if err := runReportCmd([]string{"generate", "-c", cfgPath, "--no-logs"}); err != nil {
		t.Fatalf("runReportCmd: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(reportsDir, report.DayName(now), "report.json"))
	if err != nil {
		t.Fatalf("读今天 report.json：%v", err)
	}
	var rep obs.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("解析 report.json: %v", err)
	}
	if rep.Inputs.PapaVersion == "" {
		t.Fatalf("inputs.papa_version 为空：%+v", rep.Inputs)
	}
	if rep.Inputs.Thresholds["event_max_gap"] == "" {
		t.Fatalf("inputs.thresholds.event_max_gap 为空：%+v", rep.Inputs.Thresholds)
	}
}

// --snapshot 且没有 --run：只出「最后一个事件所在本地日」的一份报告，绝不按天拆分。
func TestReportGenerateSnapshotWritesOnlyLastDay(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportsDir := filepath.Join(dir, "reports")
	now := time.Now()
	writeReportTestEvents(t, filepath.Join(logDir, "events.jsonl"), now)
	cfgPath := writeReportTestConfig(t, dir, logDir, reportsDir)

	if err := runReportCmd([]string{"generate", "-c", cfgPath, "--snapshot", "--no-logs"}); err != nil {
		t.Fatalf("runReportCmd: %v", err)
	}

	// fixture 的「今天」事件落在 now+1min（与实现取的最后事件时刻一致）；
	// 用它算目录名，避免测试恰好在 23:59 跑时窗口跨天。
	lastDay := report.DayName(now.Add(time.Minute))
	todayDir := filepath.Join(reportsDir, lastDay)
	raw, err := os.ReadFile(filepath.Join(todayDir, "report.json"))
	if err != nil {
		t.Fatalf("缺少 snapshot 日报 %s/report.json：%v", todayDir, err)
	}
	var rep obs.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("解析 report.json: %v", err)
	}
	if rep.Mode != obs.ModeSnapshot {
		t.Fatalf("report.json mode = %q, want snapshot", rep.Mode)
	}

	// 只写一份：报告根目录下只有最后一天那个目录。
	entries, err := os.ReadDir(reportsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != lastDay {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("snapshot 只该写 %s 一份，实得 %v", lastDay, names)
	}

	// 昨天（相对最后事件）不该有目录：snapshot 不按天拆分。
	prevDayDir := filepath.Join(reportsDir, report.DayName(now.Add(time.Minute).AddDate(0, 0, -1)))
	if _, err := os.Stat(prevDayDir); !os.IsNotExist(err) {
		t.Fatalf("--snapshot 不该写前一天目录 %s（err=%v）", prevDayDir, err)
	}
}
