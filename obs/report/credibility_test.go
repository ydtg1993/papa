package report

import (
	"os"
	"path/filepath"
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
