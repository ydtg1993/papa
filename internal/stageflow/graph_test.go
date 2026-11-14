package stageflow

import "testing"

func TestCompileRejectsInvalidGraph(t *testing.T) {
	for name, nodes := range map[string]map[string]Node{
		"self":          {"a": {Downstream: []string{"a"}}},
		"cycle":         {"a": {Downstream: []string{"b"}}, "b": {Downstream: []string{"a"}}},
		"unknown":       {"a": {Downstream: []string{"missing"}}},
		"duplicate":     {"a": {Downstream: []string{"b", "b"}}, "b": {}},
		"entry inbound": {"a": {Downstream: []string{"b"}}, "b": {Entry: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(nodes); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}

func TestGraphLevelsEdgesAndSnapshot(t *testing.T) {
	children := []string{"b", "d"}
	nodes := map[string]Node{"a": {Entry: true, Downstream: children}, "b": {Downstream: []string{"c"}}, "c": {Downstream: []string{"d"}}, "d": {}, "isolated": {}}
	g, err := Compile(nodes)
	if err != nil {
		t.Fatal(err)
	}
	children[0] = "isolated"
	delete(nodes, "a")
	for stage, want := range map[string]int{"a": 0, "b": 1, "c": 2, "d": 3, "isolated": 0} {
		if got, ok := g.Level(stage); !ok || got != want {
			t.Fatalf("%s level=%d,%v want %d", stage, got, ok, want)
		}
	}
	if !g.Root("a") || g.Root("isolated") || g.Root("missing") {
		t.Fatal("incorrect entry roots")
	}
	if !g.Allows("a", "b") || !g.Allows("a", "d") || g.Allows("a", "c") || g.Allows("d", "a") || g.Allows("a", "a") || g.Allows("a", "isolated") {
		t.Fatal("routing not restricted to declared edges")
	}
	if _, ok := g.Level("missing"); ok {
		t.Fatal("unknown stage has a level")
	}
}
