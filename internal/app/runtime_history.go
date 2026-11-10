package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"

	"github.com/ydtg1993/papa/v4/admin/configstore"
	"gorm.io/gorm"
)

type runtimeChange struct {
	Path          []string `json:"path"`
	Before        any      `json:"before"`
	After         any      `json:"after"`
	BeforePresent bool     `json:"before_present"`
	AfterPresent  bool     `json:"after_present"`
}

func runtimeLeaves(doc runtimeDocument) (map[string]any, map[string][]string) {
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
func runtimeChanges(current, target runtimeDocument) []runtimeChange {
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
	changes := make([]runtimeChange, 0)
	for _, k := range names {
		bv, bp := before[k]
		av, ap := after[k]
		if bp == ap && reflect.DeepEqual(bv, av) {
			continue
		}
		changes = append(changes, runtimeChange{Path: keys[k], Before: bv, After: av, BeforePresent: bp, AfterPresent: ap})
	}
	return changes
}

func patchRuntimeHistory(current, target runtimeDocument, paths [][]string) (runtimeDocument, error) {
	if len(paths) == 0 || len(paths) > 1024 {
		return runtimeDocument{}, errors.New("请选择需要恢复的字段（最多 1024 项）")
	}
	changes := map[string]runtimeChange{}
	for _, change := range runtimeChanges(current, target) {
		key, _ := json.Marshal(change.Path)
		changes[string(key)] = change
	}
	raw, _ := json.Marshal(current)
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return runtimeDocument{}, err
	}
	seen := map[string]bool{}
	for _, path := range paths {
		key, _ := json.Marshal(path)
		change, ok := changes[string(key)]
		if !ok || seen[string(key)] {
			return runtimeDocument{}, errors.New("恢复字段与当前差异不匹配，请重新预览")
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
		return runtimeDocument{}, err
	}
	return decodeRuntime(raw)
}

func (a *App) runtimeHistoryAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := a.configStore.History(r.Context(), configstore.KeyRuntimePolicy)
		if err != nil {
			writeRuntimeJSON(w, 503, map[string]any{"error": "策略历史读取失败，请检查数据库及迁移"})
			return
		}
		a.runtimeState.mu.Lock()
		defer a.runtimeState.mu.Unlock()
		writeRuntimeJSON(w, 200, map[string]any{"history": rows, "revision": a.runtimeState.revision, "retention": configstore.RuntimeHistoryLimit})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Action   string     `json:"action"`
		Revision uint64     `json:"revision"`
		Target   uint64     `json:"target"`
		Paths    [][]string `json:"paths"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 128<<10), &input); err != nil || (input.Action != "preview" && input.Action != "restore") {
		writeRuntimeJSON(w, 400, map[string]any{"error": "历史策略请求无效"})
		return
	}
	row, err := a.configStore.HistoryRevision(r.Context(), configstore.KeyRuntimePolicy, input.Target)
	if err != nil {
		status := 503
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = 404
		}
		writeRuntimeJSON(w, status, map[string]any{"error": "历史版本不可用，请刷新历史列表"})
		return
	}
	target, err := decodeRuntime(row.Document)
	if err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "历史版本格式已不兼容"})
		return
	}
	s := &a.runtimeState
	s.mu.Lock()
	if s.uncertain {
		err = a.reconcileRuntimeLocked(r.Context())
	}
	if err != nil || s.revision != input.Revision {
		s.mu.Unlock()
		writeRuntimeJSON(w, 409, map[string]any{"error": "当前版本已变化或待核对，请重新预览"})
		return
	}
	changes := runtimeChanges(s.document, target)
	if input.Action == "preview" {
		// The effective preview is recomputed against today's declarations, not
		// a stale historical effective snapshot.
		effective, _, resolveErr := a.Engine.ResolveRuntime(target.Overrides)
		poolErr := error(nil)
		if resolveErr == nil {
			poolErr = a.validateRuntimePools(effective)
		}
		warning := ""
		if resolveErr != nil || poolErr != nil {
			warning = "完整历史设置与当前站点或账号池不兼容；请仅选择仍适用的字段，保存时重新校验。"
		}
		view := map[string]any{"revision": s.revision, "target": input.Target, "changes": changes, "effective": effective, "warning": warning}
		s.mu.Unlock()
		writeRuntimeJSON(w, 200, view)
		return
	}
	document, err := patchRuntimeHistory(s.document, target, input.Paths)
	s.mu.Unlock()
	if err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	// Reuse validation, CAS persistence and publication. A concurrent edit after
	// this preview is rejected there rather than overwritten.
	raw, err := json.Marshal(struct {
		Revision uint64          `json:"revision"`
		Document runtimeDocument `json:"document"`
	}{input.Revision, document})
	if err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "恢复内容编码失败"})
		return
	}
	forward := r.Clone(r.Context())
	forward.Body = io.NopCloser(bytes.NewReader(raw))
	a.runtimeAudit(r, "runtime_policy", "restore_requested", strconv.FormatUint(input.Target, 10), nil)
	a.runtimePolicyAPI(w, forward)
}
