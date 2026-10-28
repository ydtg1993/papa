package report

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

func TestMarkdownRendersInputsSelfProof(t *testing.T) {
	rep := credibilityReport()
	rep.Mode = obs.ModeAcceptance
	rep.Verdict = obs.VerdictWarn
	rep.Inputs = obs.Inputs{
		SnapshotAt:       rep.GeneratedAt,
		PapaVersion:      "v3.8.0",
		PapaCommit:       "abc1234",
		ConfigFile:       "configs/config.yaml",
		Thresholds:       map[string]string{"stuck_after": "10m"},
		ThresholdsSource: "config",
		Events:           obs.EventCoverage{FirstTS: rep.Run.From, LastTS: rep.Run.To, MaxGap: time.Minute},
	}
	md := Markdown(rep)
	for _, want := range []string{"DB 快照时刻", "v3.8.0", "abc1234", "阈值来源：config", "最大事件间隔"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md 缺自证信息 %q：\n%s", want, md)
		}
	}
}

func TestAIBriefSnapshotSummary(t *testing.T) {
	rep := credibilityReport()
	rep.Engine.Sites = []obs.SiteState{{Key: "a", Paused: true}, {Key: "b", Paused: true}}
	rep.Sections = append(rep.Sections, obs.SectionReport{
		Key: "backlog", Title: "积压与老化（含 ETA）",
		Metrics: obs.BacklogSummary{TotalPending: 1200, TotalProcessing: 379, NonRepeatablePending: 804, ETA: "约 15h"},
	})
	b := BuildAIBrief(rep)
	if len(b.Snapshot.PausedSites) != 2 || b.Snapshot.Backlog.ETA == "" {
		t.Fatalf("AIBrief.Snapshot 缺暂停/积压摘要：%+v", b.Snapshot)
	}
}

// ai-brief.md 的「现在怎样」段只在快照有内容时出现：暂停站点与积压要能直接读到，
// 而历史/空报告不能被塞一段空标题。
func TestAIBriefMarkdownSnapshotSection(t *testing.T) {
	rep := credibilityReport()
	rep.Engine.Sites = []obs.SiteState{{Key: "a", Paused: true}}
	rep.Sections = append(rep.Sections, obs.SectionReport{
		Key: "backlog", Title: "积压与老化（含 ETA）",
		Metrics: obs.BacklogSummary{TotalPending: 1200, TotalProcessing: 379, NonRepeatablePending: 804, ETA: "约 15h"},
	})
	md := AIBriefMarkdown(rep)
	for _, want := range []string{"## 现在怎样（快照摘要）", "暂停站点：a", "活跃阶段池：0", "pending 1200", "ETA 约 15h"} {
		if !strings.Contains(md, want) {
			t.Fatalf("ai-brief.md 缺快照摘要 %q：\n%s", want, md)
		}
	}
	if strings.Contains(AIBriefMarkdown(sampleReport()), "## 现在怎样") {
		t.Fatal("没有快照内容时不该渲染「现在怎样」段")
	}
}
