package sitedecl

import "testing"

func TestPlanSitesValidatesStageGraphAndCopiesEdges(t *testing.T) {
	for _, target := range []string{"catalog", "unknown"} {
		spec := SiteSpec{Key: "synthetic", Stages: []StageSpec{{Fetcher: &namedFetcher{name: "catalog"}, WorkerCount: 1, QueueSize: 1, Entry: true, Downstream: []string{target}}}}
		if _, err := PlanSites(nil, []SiteSpec{spec}); err == nil {
			t.Fatalf("invalid target %q accepted", target)
		}
	}
	edges := []string{"detail"}
	spec := SiteSpec{Key: "synthetic", Stages: []StageSpec{
		{Fetcher: &namedFetcher{name: "catalog"}, WorkerCount: 1, QueueSize: 1, Entry: true, Downstream: edges},
		{Fetcher: &namedFetcher{name: "detail"}, WorkerCount: 1, QueueSize: 1},
	}}
	plans, err := PlanSites(nil, []SiteSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	edges[0] = "changed"
	if plans[0].Stages[0].Config.Downstream[0] != "detail" {
		t.Fatal("plan aliases caller edges")
	}
	spec.Stages[0].Downstream = []string{"detail"}
	spec.Stages[1].Entry = true
	if _, err := PlanSites(nil, []SiteSpec{spec}); err == nil {
		t.Fatal("entry with incoming edge accepted")
	}
}
