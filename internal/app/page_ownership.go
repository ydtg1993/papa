package app

import "strings"

func validatePageOwnership(pages []Page, p Page) {
	owners := map[string]string{}
	edges := map[string][]string{}
	for _, page := range append(append([]Page(nil), pages...), p) {
		for _, ref := range page.Owns {
			parts := strings.Split(ref, ":")
			if len(parts) != 2 || (parts[0] != "module" && parts[0] != "page") || !validPageKey(parts[1]) {
				panic("invalid page navigation ownership: " + ref)
			}
			if _, exists := owners[ref]; exists {
				panic("duplicate page navigation ownership: " + ref)
			}
			owners[ref] = page.Key
			if parts[0] == "page" {
				edges[page.Key] = append(edges[page.Key], parts[1])
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string)
	visit = func(key string) {
		if visiting[key] {
			panic("cyclic page navigation ownership")
		}
		if visited[key] {
			return
		}
		visiting[key] = true
		for _, target := range edges[key] {
			visit(target)
		}
		visiting[key], visited[key] = false, true
	}
	for key := range edges {
		visit(key)
	}
}
