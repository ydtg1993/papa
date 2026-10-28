package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestFailureEvidencePerGroup(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	in := &obs.Input{
		Mode: obs.ModeAcceptance, Now: now,
		Events: []events.Event{
			{Type: events.TypeTaskFailed, TS: now.Add(-20 * time.Minute), Site: "a", Stage: "episode", Kind: "retryable",
				TaskID: 42, URL: "https://a.test/play/1", Error: "create browser: Access is denied"},
			{Type: events.TypeTaskFailed, TS: now.Add(-10 * time.Minute), Site: "b", Stage: "catalog", Kind: "structure",
				TaskID: 7, URL: "https://b.test/drama/x", Error: "unsupported detail route"},
		},
		Logs: obs.LogSummary{Signatures: []obs.LogSignature{
			{Pattern: "task failed: #", Count: 1, Samples: []obs.LogSample{
				{File: "engine.log", Line: 100, Msg: "task failed: task_id=42 create browser: Access is denied"},
			}},
			{Pattern: "catalog card", Count: 1, Samples: []obs.LogSample{
				{File: "engine.log", Line: 200, Msg: "task failed: task_id=7 unsupported detail route"},
			}},
		}},
	}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var aProblem, bProblem *obs.Problem
	for i := range res.Problems {
		switch res.Problems[i].Scope {
		case "a/episode":
			aProblem = &res.Problems[i]
		case "b/catalog":
			bProblem = &res.Problems[i]
		}
	}
	if aProblem == nil || bProblem == nil {
		t.Fatalf("失败分组问题缺失：%+v", res.Problems)
	}
	if !strings.Contains(aProblem.Evidence[len(aProblem.Evidence)-1].Ref, "engine.log:100") {
		t.Fatalf("a 组证据应命中自己的日志行：%+v", aProblem.Evidence)
	}
	if !strings.Contains(bProblem.Evidence[len(bProblem.Evidence)-1].Ref, "engine.log:200") {
		t.Fatalf("b 组证据应命中自己的日志行：%+v", bProblem.Evidence)
	}
	if len(aProblem.Hypotheses) == 0 || !strings.Contains(aProblem.Hypotheses[0].Text, "权限") {
		t.Fatalf("Access is denied 应给权限假设：%+v", aProblem.Hypotheses)
	}
}

func TestSummaryCountsFailureGroups(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now, Events: []events.Event{
		{Type: events.TypeTaskFailed, TS: now, Site: "a", Stage: "catalog", Kind: "structure", TaskID: 1},
	}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "失败分组 1 个") {
		t.Fatalf("summary 应报真实失败分组数，实得 %q", res.Summary)
	}
}

// 跨天窗口里 run 起于窗口前是正常现象：不应再报「没有 run_started」warn。
func TestRunStartedBeforeWindowIsNotAWarning(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{RunIDs: []string{"r-cross-day"}, From: now.Add(-30 * time.Minute), To: now},
		Events: []events.Event{
			{Type: events.TypeTaskFinished, TS: now.Add(-time.Minute), Site: "a", Stage: "catalog"},
		}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Problems {
		if p.ID == "run-boundary-missing" {
			t.Fatalf("run 起于窗口前时不该报 run-boundary-missing：%+v", p)
		}
	}
}
