package obs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type isolationSection struct {
	call   func(string)
	result *SectionResult
}

func (s isolationSection) Key() string   { s.call("Key"); return "isolated" }
func (s isolationSection) Title() string { s.call("Title"); return "Isolated" }
func (s isolationSection) Build(context.Context, *Input) (*SectionResult, error) {
	s.call("Build")
	return s.result, nil
}

type isolationPattern struct{ call func(string) }

func (p isolationPattern) Key() string { p.call("Key"); return "isolated-pattern" }
func (p isolationPattern) Match(*Input) []Problem {
	p.call("Match")
	return []Problem{{ID: "pattern-result"}}
}

type hostilePanic struct{}

func (hostilePanic) String() string { panic("must not format panic values") }

func TestGenerateIsolatesEveryExtensionPanic(t *testing.T) {
	for _, kind := range []string{"section", "pattern"} {
		steps := []string{"Key", "Title", "Build"}
		if kind == "pattern" {
			steps = []string{"Key", "Match"}
		}
		for _, step := range steps {
			t.Run(kind+"/"+step, func(t *testing.T) {
				call := func(actual string) {
					if actual == step {
						panic(hostilePanic{})
					}
				}
				good := &fakeSection{res: &SectionResult{Checks: []Check{NewCheck("healthy", SeverityFail, true, "healthy", "")}}}
				opts := Options{Sections: []Section{good}, Patterns: []Pattern{&fakePattern{problems: []Problem{{ID: "survivor"}}}}}
				if kind == "section" {
					opts.Sections = append([]Section{isolationSection{call: call}}, opts.Sections...)
				} else {
					opts.Patterns = append([]Pattern{isolationPattern{call: call}}, opts.Patterns...)
				}
				rep, err := Generate(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if len(rep.Degraded) != 1 || !strings.Contains(rep.Degraded[0], step+" panicked") {
					t.Fatalf("degraded = %v", rep.Degraded)
				}
				if len(rep.Sections) != 1 || len(rep.Checks) != 1 || len(rep.Problems) != 1 || rep.Problems[0].ID != "survivor" {
					t.Fatalf("healthy extensions lost: %+v", rep)
				}
				if rep.Verdict != VerdictUnknown {
					t.Fatalf("partial report verdict = %s", rep.Verdict)
				}
			})
		}
	}
}

func TestGenerateCancellationStopsFurtherExtensionCalls(t *testing.T) {
	for _, kind := range []string{"section", "pattern"} {
		steps := []string{"Key", "Title", "Build"}
		if kind == "pattern" {
			steps = []string{"Key", "Match"}
		}
		for _, step := range steps {
			t.Run(kind+"/"+step, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				canceled := false
				call := func(actual string) {
					if canceled {
						t.Fatalf("called %s after cancellation", actual)
					}
					if actual == step {
						canceled = true
						cancel()
					}
				}
				never := func(string) { t.Fatal("called later extension after cancellation") }
				opts := Options{}
				if kind == "section" {
					opts.Sections = []Section{isolationSection{call: call}, isolationSection{call: never}}
					opts.Patterns = []Pattern{isolationPattern{call: never}}
				} else {
					opts.Patterns = []Pattern{isolationPattern{call: call}, isolationPattern{call: never}}
				}
				rep, err := Generate(ctx, opts)
				if rep != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("Generate = %v, %v", rep, err)
				}
			})
		}
	}
}

func TestGenerateAlreadyCanceledSkipsAllExtensions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(string) { t.Fatal("called extension with canceled context") }
	rep, err := Generate(ctx, Options{Sections: []Section{isolationSection{call: never}}, Patterns: []Pattern{isolationPattern{call: never}}})
	if rep != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate = %v, %v", rep, err)
	}
}
