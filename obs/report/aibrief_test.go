package report

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/obs"
)

func TestAIBriefIsDeterministic(t *testing.T) {
	rep := sampleReport()
	a, err := AIBriefJSON(rep)
	if err != nil {
		t.Fatal(err)
	}
	b, err := AIBriefJSON(rep)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("ai-brief.json 两次生成不一致")
	}
}

func TestAIBriefJSONHasActionableSections(t *testing.T) {
	raw, err := AIBriefJSON(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"boundaries"`, `"known_pitfalls"`, `"next_actions"`, `"problems"`, `"verdict"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("ai-brief.json 缺少 %s：\n%s", want, raw)
		}
	}
}

func TestAIBriefMarkdownSections(t *testing.T) {
	md := AIBriefMarkdown(sampleReport())
	for _, want := range []string{
		"# AI 排查任务包", "## 边界（先读）", "## 问题清单", "## 已知陷阱",
		"## 未知项", "## 下一步动作", "重跑后归零",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("ai-brief.md 缺少 %q：\n%s", want, md)
		}
	}
}

func TestWriteProducesFourFiles(t *testing.T) {
	dir := t.TempDir()
	files, err := Write(dir, sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("应写 4 个文件，实得 %v", files)
	}
}

// next_actions 上限 8（设计口径「按严重级取前 8」），且按 Problems 原顺序截断（确定性）。
func TestAIBriefNextActionsCapsAtEightInOrder(t *testing.T) {
	rep := sampleReport()
	rep.Problems = nil
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"} {
		rep.Problems = append(rep.Problems, obs.Problem{ID: id, Severity: obs.SeverityWarn})
	}
	b := BuildAIBrief(rep)
	if len(b.NextActions) != 8 {
		t.Fatalf("next_actions 应为 8 条，实得 %d：%v", len(b.NextActions), b.NextActions)
	}
	for i, want := range []string{"`p1`", "`p2`", "`p3`", "`p4`", "`p5`", "`p6`", "`p7`", "`p8`"} {
		if !strings.Contains(b.NextActions[i], want) {
			t.Fatalf("第 %d 条行动应含 %s，实得 %q", i, want, b.NextActions[i])
		}
	}
	if strings.Contains(strings.Join(b.NextActions, "\n"), "`p9`") {
		t.Fatalf("超过 8 条的部分应截断：%v", b.NextActions)
	}
}

// check 合成的问题没有假设：行动项改为按首条证据定位，不再硬写「按假设验证后修复」。
func TestAIBriefNextActionUsesEvidenceWhenNoHypothesis(t *testing.T) {
	rep := sampleReport()
	rep.Problems = []obs.Problem{
		{
			ID: "check-unfinished_tasks", Severity: obs.SeverityFail,
			Evidence:   []obs.Evidence{{Type: "check", Ref: "unfinished_tasks"}},
			Acceptance: "修复后重新生成报告，该检查通过",
		},
		{
			ID: "task-failed-s-catalog-structure", Severity: obs.SeverityWarn,
			Evidence:   []obs.Evidence{{Type: "event", Ref: "task_failed task_id=1"}},
			Hypotheses: []obs.Hypothesis{{Text: "页面结构变化", Confidence: "high", Verify: "papa rod --url x", FixHint: "fetcher"}},
			Acceptance: "重跑后归零",
		},
	}
	b := BuildAIBrief(rep)
	if len(b.NextActions) != 2 {
		t.Fatalf("应有 2 条行动：%v", b.NextActions)
	}
	if !strings.Contains(b.NextActions[0], "按证据定位后修复（unfinished_tasks）") {
		t.Fatalf("无假设的问题应按证据定位：%q", b.NextActions[0])
	}
	if strings.Contains(b.NextActions[0], "按假设验证后修复") {
		t.Fatalf("无假设的问题不该写「按假设验证后修复」：%q", b.NextActions[0])
	}
	if !strings.Contains(b.NextActions[1], "按假设验证后修复") {
		t.Fatalf("有假设的问题应保留原行动文案：%q", b.NextActions[1])
	}
	if md := AIBriefMarkdown(rep); !strings.Contains(md, "按证据定位后修复（unfinished_tasks）") {
		t.Fatalf("ai-brief.md 的行动项没带上证据定位：\n%s", md)
	}
}

// snapshot 边界说明：本份追加，且不能污染全局边界切片（不要直接 append 全局变量）。
func TestAIBriefSnapshotBoundaryIsCopy(t *testing.T) {
	baseLen := len(briefBoundaries)
	rep := sampleReport()
	rep.Mode = obs.ModeSnapshot

	b := BuildAIBrief(rep)
	if len(b.Boundaries) != baseLen+1 {
		t.Fatalf("snapshot 应多一条边界，实得 %d 条：%v", len(b.Boundaries), b.Boundaries)
	}
	if b.Boundaries[len(b.Boundaries)-1] != snapshotBoundary {
		t.Fatalf("最后一条边界应是 snapshot 说明，实得 %q", b.Boundaries[len(b.Boundaries)-1])
	}
	if len(briefBoundaries) != baseLen {
		t.Fatalf("全局边界切片被改写了：%v", briefBoundaries)
	}
	// 再生成一次不应重复追加（共享底层数组会表现为 5 条、6 条…）。
	if again := BuildAIBrief(rep); len(again.Boundaries) != baseLen+1 {
		t.Fatalf("重复生成把 snapshot 边界叠了：%v", again.Boundaries)
	}
	// acceptance 不追加。
	rep.Mode = obs.ModeAcceptance
	if acc := BuildAIBrief(rep); len(acc.Boundaries) != baseLen {
		t.Fatalf("acceptance 不该有 snapshot 边界：%v", acc.Boundaries)
	}
	if md := AIBriefMarkdown(rep); strings.Contains(md, snapshotBoundary) {
		t.Fatalf("acceptance 的 ai-brief.md 不该出现 snapshot 边界：\n%s", md)
	}
	rep.Mode = obs.ModeSnapshot
	if md := AIBriefMarkdown(rep); !strings.Contains(md, snapshotBoundary) {
		t.Fatalf("snapshot 的 ai-brief.md 应渲染边界说明：\n%s", md)
	}
}
