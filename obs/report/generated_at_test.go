package report

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

// 报告头部必须标注生成时间（= 最近一次更新时间）：定时报告每天覆盖同一目录时，
// 靠这个字段判断"手上这份是什么时候生成的"。
func TestMarkdownAndAIBriefShowGeneratedAt(t *testing.T) {
	at := time.Date(2026, 10, 28, 23, 30, 0, 0, time.Local)
	rep := &obs.Report{
		SchemaVersion: obs.SchemaVersion,
		GeneratedAt:   at,
		Verdict:       obs.VerdictPass,
		Run:           obs.RunMeta{RunIDs: []string{"r-g"}},
	}

	md := Markdown(rep)
	if !strings.Contains(md, "- 生成时间：2026-10-28 23:30:00（最近一次更新）") {
		t.Fatalf("report.md 缺少生成时间：\n%s", md)
	}
	brief := AIBriefMarkdown(rep)
	if !strings.Contains(brief, "- 生成时间：2026-10-28 23:30:00（最近一次更新）") {
		t.Fatalf("ai-brief.md 缺少生成时间：\n%s", brief)
	}
	if got := BuildAIBrief(rep).GeneratedAt; got != "2026-10-28 23:30:00" {
		t.Fatalf("AI brief GeneratedAt = %q", got)
	}
}

// 没设置生成时间时显示 "-"，不能 panic / 显示 0001 年。
func TestMarkdownGeneratedAtZeroIsDash(t *testing.T) {
	md := Markdown(&obs.Report{SchemaVersion: obs.SchemaVersion, Verdict: obs.VerdictPass})
	if !strings.Contains(md, "- 生成时间：-（最近一次更新）") {
		t.Fatalf("零值生成时间应渲染为 '-':\n%s", md)
	}
}
