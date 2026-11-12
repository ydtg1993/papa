package runtimecontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
)

type Change struct {
	Path          []string `json:"path"`
	Before        any      `json:"before"`
	After         any      `json:"after"`
	BeforePresent bool     `json:"before_present"`
	AfterPresent  bool     `json:"after_present"`
}

func runtimeLeaves(doc Document) (map[string]any, map[string][]string) {
	raw, _ := json.Marshal(doc)
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	leaves, paths := map[string]any{}, map[string][]string{}
	var visit func(any, []string)
	visit = func(v any, path []string) {
		if object, ok := v.(map[string]any); ok {
			for k, child := range object {
				visit(child, append(append([]string(nil), path...), k))
			}
			return
		}
		key, _ := json.Marshal(path)
		leaves[string(key)] = v
		paths[string(key)] = path
	}
	visit(value["overrides"], []string{"overrides"})
	return leaves, paths
}

// Historical restoration is a selected-field patch against a pinned current
// revision. Unselected fields (including new settings) are preserved.
func Changes(current, target Document) []Change {
	before, beforePaths := runtimeLeaves(current)
	after, afterPaths := runtimeLeaves(target)
	keys := map[string][]string{}
	for k, path := range beforePaths {
		keys[k] = path
	}
	for k, path := range afterPaths {
		keys[k] = path
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	changes := make([]Change, 0)
	for _, k := range names {
		bv, bp := before[k]
		av, ap := after[k]
		if bp == ap && reflect.DeepEqual(bv, av) {
			continue
		}
		changes = append(changes, Change{Path: keys[k], Before: bv, After: av, BeforePresent: bp, AfterPresent: ap})
	}
	return changes
}

func PatchHistory(current, target Document, paths [][]string) (Document, error) {
	if len(paths) == 0 || len(paths) > 1024 {
		return Document{}, errors.New("请选择需要恢复的字段（最多 1024 项）")
	}
	changes := map[string]Change{}
	for _, change := range Changes(current, target) {
		key, _ := json.Marshal(change.Path)
		changes[string(key)] = change
	}
	raw, _ := json.Marshal(current)
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return Document{}, err
	}
	seen := map[string]bool{}
	for _, path := range paths {
		key, _ := json.Marshal(path)
		change, ok := changes[string(key)]
		if !ok || seen[string(key)] {
			return Document{}, errors.New("恢复字段与当前差异不匹配，请重新预览")
		}
		seen[string(key)] = true
		object := root
		for _, part := range path[:len(path)-1] {
			child, ok := object[part].(map[string]any)
			if !ok {
				child = map[string]any{}
				object[part] = child
			}
			object = child
		}
		if change.AfterPresent {
			object[path[len(path)-1]] = change.After
		} else {
			delete(object, path[len(path)-1])
		}
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return Document{}, err
	}
	return Decode(raw)
}
