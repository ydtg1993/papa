package app

import (
	"strings"
	"testing"
)

func flowSite() SiteSpec {
	return SiteSpec{Key: "flow", Stages: []StageSpec{
		{Fetcher: namedFetcher{"catalog"}, WorkerCount: 1, QueueSize: 1, Entry: true, Downstream: []string{"detail"}},
		{Fetcher: namedFetcher{"detail"}, WorkerCount: 1, QueueSize: 1, Downstream: []string{"episode"}},
		{Fetcher: namedFetcher{"episode"}, WorkerCount: 1, QueueSize: 1},
	}}
}

func TestStageFlowDeclarationsRejectInvalidGraphs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*SiteSpec)
		want string
	}{
		{"self", func(s *SiteSpec) { s.Stages[0].Downstream = []string{"catalog"} }, "self submission"},
		{"unknown", func(s *SiteSpec) { s.Stages[0].Downstream = []string{"missing"} }, "not registered"},
		{"cycle", func(s *SiteSpec) { s.Stages[2].Downstream = []string{"catalog"} }, "cyclic"},
		{"duplicate", func(s *SiteSpec) { s.Stages[0].Downstream = []string{"detail", "detail"} }, "duplicate downstream"},
		{"cross_site_name", func(s *SiteSpec) { s.Stages[0].Downstream = []string{"other:detail"} }, "invalid downstream"},
		{"whitespace", func(s *SiteSpec) { s.Stages[0].Downstream = []string{" detail"} }, "invalid downstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := flowSite()
			tc.edit(&s)
			if err := CheckSites(nil, s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestStageFlowDeclarationPlansAreStrictAndCopied(t *testing.T) {
	s := flowSite()
	plans, err := planSites(nil, []SiteSpec{s})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range plans[0].stages {
		if !stage.cfg.StrictFlow {
			t.Fatal("application declaration did not enable strict flow")
		}
	}
	s.Stages[0].Downstream[0] = "episode"
	if got := plans[0].stages[0].cfg.Downstream[0]; got != "detail" {
		t.Fatalf("declaration mutation changed graph to %s", got)
	}
	if len(plans[0].stages[2].cfg.Downstream) != 0 {
		t.Fatal("terminal stage has downstream")
	}
}
