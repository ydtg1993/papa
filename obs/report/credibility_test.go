package report

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

func credibilityReport() *obs.Report {
	at := time.Date(2026, 10, 29, 0, 27, 41, 0, time.Local)
	return &obs.Report{
		SchemaVersion: obs.SchemaVersion,
		GeneratedAt:   at,
		Verdict:       obs.VerdictUnknown,
		Mode:          obs.ModeSnapshot,
		VerdictReason: "引擎暂停/空闲，快照模式不评估运行健康",
		Run:           obs.RunMeta{From: at.Add(-27 * time.Minute), To: at},
		Engine:        obs.EngineState{Source: "engine", ActivePools: 0},
		Checks: []obs.Check{
			obs.NewCheck("event_gaps", obs.SeverityWarn, true, "缺口 0", ""),
			{ID: "stuck_processing", Severity: obs.SeverityFail, Skipped: true, SkipReason: "引擎暂停", Message: "441 条"},
		},
	}
}

func TestMarkdownRendersModeSkipAndUnknown(t *testing.T) {
	md := Markdown(credibilityReport())
	for _, want := range []string{"**判定：未判定**", "模式：snapshot", "➖ 不适用", "引擎暂停"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md 缺少 %q：\n%s", want, md)
		}
	}
}

func TestAIBriefListsSkippedChecks(t *testing.T) {
	b := BuildAIBrief(credibilityReport())
	if b.Mode != "snapshot" || b.VerdictReason == "" {
		t.Fatalf("AIBrief 缺 mode/verdict_reason：%+v", b)
	}
	joined := strings.Join(b.Unknowns, "\n")
	if !strings.Contains(joined, "stuck_processing") || !strings.Contains(joined, "引擎暂停") {
		t.Fatalf("不适用项应进 unknowns：%v", b.Unknowns)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	rep := credibilityReport()
	if _, err := Write(dir, rep); err != nil {
		t.Fatal(err)
	}
	rep.GeneratedAt = rep.GeneratedAt.Add(time.Minute)
	if _, err := Write(dir, rep); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "2026-10-29T00:28:41") {
		t.Fatalf("第二次写入没生效：%s", raw)
	}
}

// 原子写不应把产物权限从 0644 改成 0600（旧实现是 os.WriteFile(..., 0o644)）。
// Windows 的 chmod 语义有限，权限断言只在类 Unix 上有效。
func TestWriteKeepsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 chmod 语义有限，权限断言只在类 Unix 上有效")
	}
	dir := t.TempDir()
	if _, err := Write(dir, credibilityReport()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Fatalf("%s 权限 = %04o, want 0644", name, got)
		}
	}
}

// 历史/合成报告没有 mode 时，ai-brief.md 也要显示占位而不是空行。
func TestAIBriefMarkdownEmptyMode(t *testing.T) {
	rep := credibilityReport()
	rep.Mode = ""
	rep.VerdictReason = ""
	md := AIBriefMarkdown(rep)
	if !strings.Contains(md, "- 模式：-（历史报告）") {
		t.Fatalf("空模式应显示历史报告占位：\n%s", md)
	}
}
