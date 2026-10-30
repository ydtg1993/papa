package report

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

func sampleReport() *obs.Report {
	return &obs.Report{
		SchemaVersion: obs.SchemaVersion,
		Verdict:       obs.VerdictWarn,
		Run:           obs.RunMeta{RunIDs: []string{"r-1"}, From: time.Unix(0, 0), To: time.Unix(3600, 0)},
		EventMeta:     obs.EventMeta{Count: 12, Files: 1, Gaps: 0},
		Checks: []obs.Check{
			obs.NewCheck("stuck_processing", obs.SeverityFail, true, "0 条", "报告口径"),
			obs.NewCheck("crawl_fail_rate", obs.SeverityWarn, false, "6.0%", "阈值 ≤5%"),
		},
		Problems: []obs.Problem{{
			ID: "task-failed-s-catalog-structure", Severity: obs.SeverityWarn,
			Title: "s/catalog 终态失败 1 次（kind=structure）", Count: 1,
			Evidence:   []obs.Evidence{{Type: "log", Ref: "engine.log:12", Excerpt: "task failed: selector miss"}},
			Hypotheses: []obs.Hypothesis{{Text: "页面结构变化", Confidence: "high", Verify: "papa rod --url x", FixHint: "fetcher"}},
			Acceptance: "重跑后归零",
		}},
		Sections: []obs.SectionReport{{
			Key: "framework", Title: "框架运行概览", Summary: "事件 12 条",
			Tables: []obs.Table{{Title: "运行边界", Columns: []string{"项", "值"}, Rows: [][]string{{"事件条数", "12"}}}},
		}},
	}
}

func TestMarkdownContainsKeyParts(t *testing.T) {
	md := Markdown(sampleReport())
	for _, want := range []string{
		"# 运行报告", "带风险通过", "## 判定项", "## 问题清单",
		"engine.log:12", "框架运行概览", "| 项 | 值 |", "只读生成",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("报告缺少 %q：\n%s", want, md)
		}
	}
}

func TestJSONRoundTripFields(t *testing.T) {
	raw, err := JSON(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"schema_version": 1`, `"verdict": "warn"`, `"checks"`, `"problems"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("JSON 缺少 %s：\n%s", want, string(raw))
		}
	}
}

func TestSortedRunIDs(t *testing.T) {
	got := SortedRunIDs([]string{"b", "a", "b", ""})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("SortedRunIDs = %v", got)
	}
}

// info 级检查表达「状态」而不是通过/失败：结果列统一 ℹ️，避免
// 「engine_idle ❌ 未通过」这种结果列与说明列互相打脸的行。
func TestCheckResultTextInfoIsStateNotPassFail(t *testing.T) {
	cases := []struct {
		name string
		c    obs.Check
		want string
	}{
		{"info passed", obs.NewCheck("stage_pools", obs.SeverityInfo, true, "活跃阶段池 15 个", "0 = 暂停或空闲"), "ℹ️ 状态"},
		{"info not passed", obs.NewCheck("engine_idle", obs.SeverityInfo, false, "引擎空闲（无活跃池且窗口内无任务事件）", "空闲本身不是故障"), "ℹ️ 状态"},
		{"warn passed", obs.NewCheck("event_gaps", obs.SeverityWarn, true, "缺口 0", ""), "✅ 通过"},
		{"warn failed", obs.NewCheck("queue_drained", obs.SeverityWarn, false, "2022 条", ""), "❌ 未通过"},
		{"fail failed", obs.NewCheck("unfinished_tasks", obs.SeverityFail, false, "2267 条", ""), "❌ 未通过"},
	}
	for _, tt := range cases {
		if got := checkResultText(tt.c); got != tt.want {
			t.Fatalf("%s: checkResultText = %q, want %q", tt.name, got, tt.want)
		}
	}
	skipped := obs.Check{ID: "stuck_processing", Severity: obs.SeverityFail, Skipped: true, SkipReason: "引擎暂停"}
	if got := checkResultText(skipped); got != "➖ 不适用（引擎暂停）" {
		t.Fatalf("skipped 结果列不应变化：%q", got)
	}
}

// 渲染出来后，判不了定的 info 行既不是红叉也不是绿勾。
func TestMarkdownInfoCheckRendersStateColumn(t *testing.T) {
	rep := sampleReport()
	rep.Checks = append(rep.Checks, obs.NewCheck("engine_idle", obs.SeverityInfo, false,
		"引擎空闲（无活跃池且窗口内无任务事件）", "空闲本身不是故障，只说明这一窗口没有产出"))
	md := Markdown(rep)
	var row string
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "`engine_idle`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("判定项表缺 engine_idle 行：\n%s", md)
	}
	if !strings.Contains(row, "ℹ️ 状态") {
		t.Fatalf("info 检查结果列应为 ℹ️ 状态：%q", row)
	}
	if strings.Contains(row, "❌") || strings.Contains(row, "✅") {
		t.Fatalf("info 检查不该出现红叉/绿勾：%q", row)
	}
}
