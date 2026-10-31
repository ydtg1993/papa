package obs

import (
	"strings"
	"testing"
)

func TestVerdictOfSkipsSkippedChecks(t *testing.T) {
	got := VerdictOf([]Check{
		{ID: "stuck", Severity: SeverityFail, Passed: false, Skipped: true, SkipReason: "暂停"},
	})
	if got != VerdictPass {
		t.Fatalf("skipped fail 检查不该影响判定，实得 %q", got)
	}
}

func TestApplyVerdictPolicy(t *testing.T) {
	cases := []struct {
		name string
		rep  *Report
		want Verdict
	}{
		{"degraded+pass→unknown", &Report{Verdict: VerdictPass, Degraded: []string{"数据库连接失败"},
			Checks: []Check{{ID: "ok", Severity: SeverityFail, Passed: true}}}, VerdictUnknown},
		{"no applicable checks→unknown", &Report{Verdict: VerdictPass, Checks: []Check{{ID: "x", Severity: SeverityFail, Skipped: true}}}, VerdictUnknown},
		{"snapshot+engine inactive+pass→pass", &Report{Verdict: VerdictPass, Mode: ModeSnapshot,
			Engine: EngineState{Source: "engine", ActivePools: 0},
			Checks: []Check{{ID: "ok", Severity: SeverityFail, Passed: true}}}, VerdictPass},
		{"acceptance+pass→pass", &Report{Verdict: VerdictPass, Mode: ModeAcceptance,
			Checks: []Check{{ID: "ok", Severity: SeverityFail, Passed: true}}}, VerdictPass},
		{"fail 不被 unknown 覆盖", &Report{Verdict: VerdictFail, Degraded: []string{"x"},
			Checks: []Check{{ID: "stuck", Severity: SeverityFail, Passed: false}}}, VerdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyVerdictPolicy(tc.rep)
			if tc.rep.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q（reason=%q）", tc.rep.Verdict, tc.want, tc.rep.VerdictReason)
			}
			if (tc.want == VerdictUnknown || tc.want == VerdictFail) && tc.rep.VerdictReason == "" {
				t.Fatal("fail/unknown 必须有 verdict_reason")
			}
		})
	}
}

func TestSynthesizeProblemsForFailedChecks(t *testing.T) {
	rep := &Report{Checks: []Check{
		{ID: "stuck_processing", Severity: SeverityFail, Passed: false, Message: "441 条"},
		{ID: "ok", Severity: SeverityFail, Passed: true},
	}}
	synthesizeProblems(rep)
	if len(rep.Problems) != 1 {
		t.Fatalf("应合成 1 条 Problem，实得 %d：%+v", len(rep.Problems), rep.Problems)
	}
	p := rep.Problems[0]
	if p.ID != "check-stuck_processing" || p.Origin != "check" || len(p.Evidence) == 0 {
		t.Fatalf("合成 Problem 内容不对：%+v", p)
	}
	if !strings.Contains(p.Title, "441") {
		t.Fatalf("合成 Problem 标题应带检查 message：%+v", p)
	}
}

func TestEngineStateInactive(t *testing.T) {
	cases := []struct {
		st   EngineState
		want bool
	}{
		{EngineState{Source: "engine", ActivePools: 0}, true},
		// 有阶段池且站点全部暂停 → 仍算暂停（设计：无池 或 全部站点暂停）。
		{EngineState{Source: "engine", ActivePools: 2, Sites: []SiteState{{Key: "a", Paused: true}}}, true},
		// 有站点未暂停 → 引擎正在跑活，不算暂停。
		{EngineState{Source: "engine", ActivePools: 2, Sites: []SiteState{{Key: "a", Paused: true}, {Key: "b"}}}, false},
		{EngineState{Source: "engine", Sites: []SiteState{{Key: "a", Paused: true}, {Key: "b", Paused: true}}}, true},
		{EngineState{Source: "none"}, false},
	}
	for i, tc := range cases {
		if got := tc.st.Inactive(); got != tc.want {
			t.Fatalf("case %d: Inactive() = %v, want %v（%+v）", i, got, tc.want, tc.st)
		}
	}
}
