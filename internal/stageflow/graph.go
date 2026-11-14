// Package stageflow compiles immutable, site-local stage routing graphs.
// It owns graph validation and levels, with no runtime or Engine dependencies.
package stageflow

import (
	"fmt"
	"sort"
)

type Node struct {
	Downstream []string
	Entry      bool
}

type Graph struct {
	edges  map[string]map[string]struct{}
	roots  map[string]bool
	levels map[string]int
}

// Compile snapshots the declarations and rejects invalid edges and cycles.
// Levels are the longest distance from any zero-indegree stage; every edge
// therefore strictly increases level, including edges that skip a level.
func Compile(nodes map[string]Node) (*Graph, error) {
	g := &Graph{edges: make(map[string]map[string]struct{}, len(nodes)), roots: make(map[string]bool), levels: make(map[string]int, len(nodes))}
	names := make([]string, 0, len(nodes))
	indegree := make(map[string]int, len(nodes))
	for name := range nodes {
		names = append(names, name)
		indegree[name] = 0
		g.levels[name] = 0
	}
	sort.Strings(names)
	for _, source := range names {
		g.edges[source] = make(map[string]struct{})
		for _, target := range nodes[source].Downstream {
			if source == target {
				return nil, fmt.Errorf("stage %q has a self edge", source)
			}
			if _, ok := nodes[target]; !ok {
				return nil, fmt.Errorf("stage %q references unknown downstream stage %q", source, target)
			}
			if _, ok := g.edges[source][target]; ok {
				return nil, fmt.Errorf("stage %q has duplicate downstream edge %q", source, target)
			}
			g.edges[source][target] = struct{}{}
			indegree[target]++
		}
	}
	queue := make([]string, 0, len(nodes))
	for _, name := range names {
		if nodes[name].Entry && indegree[name] != 0 {
			return nil, fmt.Errorf("entry stage %q has incoming edges", name)
		}
		if indegree[name] == 0 {
			queue = append(queue, name)
			g.roots[name] = nodes[name].Entry
		}
	}
	visited := 0
	for len(queue) > 0 {
		source := queue[0]
		queue = queue[1:]
		visited++
		for target := range g.edges[source] {
			if level := g.levels[source] + 1; level > g.levels[target] {
				g.levels[target] = level
			}
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if visited != len(nodes) {
		return nil, fmt.Errorf("stage graph contains a cycle")
	}
	return g, nil
}

// Allows reports only explicitly declared edges, never transitive reachability.
func (g *Graph) Allows(source, target string) bool {
	if g == nil {
		return false
	}
	_, ok := g.edges[source][target]
	return ok
}

// Root reports whether a stage is an entry with no incoming edges.
func (g *Graph) Root(stage string) bool { return g != nil && g.roots[stage] }

func (g *Graph) Level(stage string) (int, bool) {
	if g == nil {
		return 0, false
	}
	level, ok := g.levels[stage]
	return level, ok
}
