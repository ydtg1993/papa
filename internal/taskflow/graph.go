// Package taskflow validates the declared directed acyclic stage graph.
// It owns no execution state and does not depend on the engine or storage.
package taskflow

import (
	"fmt"
	"sort"
	"strings"
)

// Validate checks direct edges within one site. Empty downstream means terminal.
func Validate(site string, edges map[string][]string) error {
	names := make([]string, 0, len(edges))
	for name := range edges {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, source := range names {
		seen := map[string]bool{}
		for _, target := range edges[source] {
			if target == "" || strings.TrimSpace(target) != target || strings.Contains(target, ":") {
				return fmt.Errorf("site %q stage %q: invalid downstream stage %q", site, source, target)
			}
			if target == source {
				return fmt.Errorf("site %q stage %q: self submission is forbidden", site, source)
			}
			if seen[target] {
				return fmt.Errorf("site %q stage %q: duplicate downstream %q", site, source, target)
			}
			seen[target] = true
			if _, ok := edges[target]; !ok {
				return fmt.Errorf("site %q stage %q: downstream %q is not registered", site, source, target)
			}
		}
	}
	color := map[string]uint8{}
	var visit func(string) error
	visit = func(source string) error {
		if color[source] == 1 {
			return fmt.Errorf("site %q: cyclic downstream flow at stage %q", site, source)
		}
		if color[source] == 2 {
			return nil
		}
		color[source] = 1
		for _, target := range edges[source] {
			if err := visit(target); err != nil {
				return err
			}
		}
		color[source] = 2
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}
