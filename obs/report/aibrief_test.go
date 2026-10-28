package report

import (
	"strings"
	"testing"
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
