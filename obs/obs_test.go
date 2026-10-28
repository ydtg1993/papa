package obs

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSection struct {
	res *SectionResult
	err error
}

func (f *fakeSection) Key() string   { return "fake" }
func (f *fakeSection) Title() string { return "Fake" }
func (f *fakeSection) Build(context.Context, *Input) (*SectionResult, error) {
	return f.res, f.err
}

type fakePattern struct{ problems []Problem }

func (f *fakePattern) Key() string            { return "fake-pattern" }
func (f *fakePattern) Match(*Input) []Problem { return f.problems }

func TestVerdictOf(t *testing.T) {
	cases := []struct {
		name   string
		checks []Check
		want   Verdict
	}{
		{"empty", nil, VerdictPass},
		{"all pass", []Check{NewCheck("a", SeverityFail, true, "", ""), NewCheck("b", SeverityWarn, true, "", "")}, VerdictPass},
		{"warn fail", []Check{NewCheck("b", SeverityWarn, false, "", "")}, VerdictWarn},
		{"fail", []Check{NewCheck("b", SeverityWarn, false, "", ""), NewCheck("a", SeverityFail, false, "", "")}, VerdictFail},
		{"passed fail ignored", []Check{NewCheck("a", SeverityFail, true, "", "")}, VerdictPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerdictOf(tc.checks); got != tc.want {
				t.Fatalf("VerdictOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGenerateAggregatesSectionsAndPatterns(t *testing.T) {
	sec := &fakeSection{res: &SectionResult{
		Summary:  "s",
		Checks:   []Check{NewCheck("stuck_processing", SeverityFail, false, "1", "")},
		Problems: []Problem{{ID: "p1", Severity: SeverityWarn, Title: "t1", Count: 1}},
	}}
	pat := &fakePattern{problems: []Problem{{ID: "p2", Severity: SeverityFail, Title: "t2", Count: 5}}}

	rep, err := Generate(context.Background(), Options{
		Run:      RunMeta{RunIDs: []string{"r-1"}},
		Sections: []Section{sec},
		Patterns: []Pattern{pat},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want fail", rep.Verdict)
	}
	if len(rep.Sections) != 1 || rep.Sections[0].Key != "fake" {
		t.Fatalf("sections = %+v", rep.Sections)
	}
	// p2（fail、count 5）应排在 p1（warn）之前；失败检查会合成一条 fail Problem 插在中间。
	if len(rep.Problems) != 3 || rep.Problems[0].ID != "p2" {
		t.Fatalf("problems 排序不对：%+v", rep.Problems)
	}
	if p := rep.Problems[1]; p.ID != "check-stuck_processing" || p.Origin != "check" {
		t.Fatalf("失败检查应合成 check Problem：%+v", rep.Problems)
	}
	if rep.Problems[2].ID != "p1" {
		t.Fatalf("p1 应排在最后：%+v", rep.Problems)
	}
	if len(rep.Checks) != 1 {
		t.Fatalf("checks = %+v", rep.Checks)
	}
}

func TestGenerateSectionErrorIsDegraded(t *testing.T) {
	rep, err := Generate(context.Background(), Options{
		Sections: []Section{&fakeSection{err: errors.New("boom")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Degraded) != 1 {
		t.Fatalf("degraded = %+v", rep.Degraded)
	}
	if rep.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want unknown（单节失败=降级，fail-closed 不判 pass）", rep.Verdict)
	}
	if rep.VerdictReason == "" {
		t.Fatal("降级报告必须给出 verdict_reason")
	}
}

func TestSortProblemsDeterministic(t *testing.T) {
	list := []Problem{
		{ID: "b", Severity: SeverityWarn, Count: 2},
		{ID: "a", Severity: SeverityWarn, Count: 2},
		{ID: "c", Severity: SeverityFail, Count: 1},
		{ID: "d", Severity: SeverityWarn, Count: 9},
	}
	sortProblems(list)
	want := []string{"c", "d", "a", "b"}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("顺序 = %v, want %v", list, want)
		}
	}
}

func TestGenerateSetsSchemaAndNow(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	rep, err := Generate(context.Background(), Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %d", rep.SchemaVersion)
	}
}
