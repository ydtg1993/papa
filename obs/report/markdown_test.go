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
