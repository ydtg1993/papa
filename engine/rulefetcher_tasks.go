package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/discovery"
)

const ruleMetaLimit = 2 << 20

type ruleCheckpoint struct {
	MaxPages int      `json:"max_pages"`
	MaxItems int      `json:"max_items"`
	Requests []string `json:"requests,omitempty"`
	Bodies   []string `json:"bodies,omitempty"`
	Items    []string `json:"items,omitempty"`
}

func ruleDigest(v []byte) string { sum := sha256.Sum256(v); return hex.EncodeToString(sum[:]) }
func ruleJSON(v any) string      { b, _ := json.Marshal(v); return string(b) }
func ruleDecode(s string, out any) error {
	if len(s) == 0 || len(s) > ruleMetaLimit {
		return fmt.Errorf("rule task: missing or oversized metadata")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("rule task: malformed metadata")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("rule task: trailing metadata")
	}
	return nil
}
func ruleReserved(k string) bool {
	return strings.HasPrefix(k, "_rule_") || k == "rule_id" || k == "rule_version"
}

func (f *RuleFetcher) allowedURL(raw string) (string, error) {
	u, err := discovery.CanonicalURL(raw, "")
	if err != nil {
		return "", fmt.Errorf("rule task: absolute HTTP(S) URL required")
	}
	for _, origin := range f.config.Origins {
		if discovery.SameOrigin(origin, u) {
			return u, nil
		}
	}
	return "", fmt.Errorf("rule task: URL outside configured origins")
}

func (f *RuleFetcher) NewTask(request RuleRequest, meta map[string]string) (*Task, error) {
	return f.NewTaskContext(context.Background(), request, meta)
}

// NewTaskContext pins the current publication in one read before constructing a
// task. Use a bounded context when Catalog accesses a database. Publishing later
// does not rebind this task or any children it creates.
func (f *RuleFetcher) NewTaskContext(ctx context.Context, request RuleRequest, meta map[string]string) (*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for key := range meta {
		if ruleReserved(key) {
			return nil, fmt.Errorf("rule task: reserved metadata key")
		}
	}
	bindings := f.config.Bindings
	if f.config.Catalog != nil {
		snapshot, err := f.config.Catalog.Snapshot(ctx, f.config.Site)
		if err != nil {
			return nil, err
		}
		if snapshot.Site != f.config.Site || snapshot.ID == "" || len(snapshot.ID) > 128 || snapshot.Generation == 0 {
			return nil, fmt.Errorf("rule task: invalid catalog publication")
		}
		if len(snapshot.Bindings) == 0 || len(snapshot.Bindings) > 32 {
			return nil, fmt.Errorf("rule task: catalog binding count outside budget")
		}
		bindings = make(map[string]RuleRef, len(snapshot.Bindings))
		for stage, ref := range snapshot.Bindings {
			bindings[stage] = RuleRef{ID: ref.ID, Version: ref.Version}
		}
		if err := f.checkCatalogBindings(ctx, bindings); err != nil {
			return nil, err
		}
		meta = maps.Clone(meta)
		if meta == nil {
			meta = map[string]string{}
		}
		meta[ruleReleaseMeta] = ruleJSON(ruleReleaseIdentity{ID: snapshot.ID, Generation: snapshot.Generation})
	}
	checkpoint := ruleCheckpoint{MaxPages: f.config.MaxPages, MaxItems: f.config.MaxItems}
	return f.makeTask(f.config.Stage, request, meta, bindings, f.config.AccountID, checkpoint)
}

func (f *RuleFetcher) makeTask(stage string, request RuleRequest, meta map[string]string, bindings map[string]RuleRef, account string, checkpoint ruleCheckpoint) (*Task, error) {
	task, err := f.makeTaskIdentity(stage, request, meta, bindings, account, checkpoint)
	if err != nil {
		return nil, err
	}
	if f.config.Registry != nil {
		ref := bindings[stage]
		if _, err := f.config.Registry.Verified(ref.ID, ref.Version); err != nil {
			return nil, err
		}
	}
	return task, nil
}

// makeTaskIdentity validates and rebuilds persisted identity without requiring
// the rule version to be loaded. Restored tasks must pass this check before a
// missing version can safely be attributed to their quality observation.
func (f *RuleFetcher) makeTaskIdentity(stage string, request RuleRequest, meta map[string]string, bindings map[string]RuleRef, account string, checkpoint ruleCheckpoint) (*Task, error) {
	var err error
	request.URL, err = f.allowedURL(request.URL)
	if err != nil {
		return nil, err
	}
	if len(request.State) > crawlrule.MaxNextCursorBytes {
		return nil, fmt.Errorf("rule task: cursor exceeds 16 KiB")
	}
	ref, ok := bindings[stage]
	if !ok {
		return nil, fmt.Errorf("rule task: target stage has no pinned rule")
	}
	releaseText := meta[ruleReleaseMeta]
	batch, hasBatch := meta[ruleBatchMeta]
	if hasBatch && !validRuleBatch(batch) {
		return nil, fmt.Errorf("rule task: invalid batch identity")
	}
	if _, err := decodeRuleRelease(releaseText); err != nil {
		return nil, err
	}
	values := maps.Clone(meta)
	if values == nil {
		values = map[string]string{}
	}
	for k := range values {
		if ruleReserved(k) {
			delete(values, k)
		}
	}
	values["rule_id"], values["rule_version"] = ref.ID, ref.Version
	values[ruleBindingsMeta], values[ruleRequestMeta], values[ruleAccountMeta] = ruleJSON(bindings), ruleJSON(request), account
	if releaseText != "" {
		values[ruleReleaseMeta] = releaseText
	}
	if batch != "" {
		values[ruleBatchMeta] = batch
	}
	// The fragment is a queue identity only; HTTP and Rod always use request.URL.
	// Existing database URL uniqueness therefore also distinguishes POST cursors,
	// account partitions and versions, without changing ordinary Task semantics.
	identity := ruleDigest([]byte(ruleJSON(values)))
	taskURL := request.URL + "#papa-rule=" + identity
	// crawler_tasks.url is MySQL TEXT (65,535 bytes), not an unbounded string.
	if len(taskURL) > 65535 {
		return nil, fmt.Errorf("rule task: URL exceeds database TEXT limit")
	}
	values[ruleCheckpointMeta] = ruleJSON(checkpoint)
	if len(ruleJSON(values)) > ruleMetaLimit {
		return nil, fmt.Errorf("rule task: metadata exceeds 2 MiB")
	}
	return &Task{Site: f.config.Site, Stage: stage, URL: taskURL, Meta: values}, nil
}

func validRuleBatch(batch string) bool {
	if len(batch) != 64 {
		return false
	}
	for _, c := range batch {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func cloneRuleTask(t *Task) *Task { copied := *t; copied.Meta = maps.Clone(t.Meta); return &copied }

func (f *RuleFetcher) taskState(task *Task) (RuleRequest, map[string]RuleRef, ruleCheckpoint, error) {
	var req RuleRequest
	var bindings map[string]RuleRef
	var cp ruleCheckpoint
	fail := func() (RuleRequest, map[string]RuleRef, ruleCheckpoint, error) {
		return req, nil, cp, fmt.Errorf("rule task: invalid pinned task state")
	}
	if task == nil || task.Site != f.config.Site || task.Stage != f.config.Stage || len(ruleJSON(task.Meta)) > ruleMetaLimit {
		return fail()
	}
	if ruleDecode(task.Meta[ruleRequestMeta], &req) != nil || ruleDecode(task.Meta[ruleBindingsMeta], &bindings) != nil || ruleDecode(task.Meta[ruleCheckpointMeta], &cp) != nil {
		return fail()
	}
	if len(bindings) == 0 || len(bindings) > 32 {
		return fail()
	}
	for stage, ref := range bindings {
		if stage == "" || ref.ID == "" || ref.Version == "" || len(stage) > 128 || len(ref.ID) > 128 || len(ref.Version) > 128 {
			return fail()
		}
	}
	for _, link := range f.config.Links {
		if _, exists := bindings[link.Stage]; !exists {
			return fail()
		}
	}
	ref, ok := bindings[task.Stage]
	if !ok || ref.ID != task.Meta["rule_id"] || ref.Version != task.Meta["rule_version"] {
		return fail()
	}
	if cp.MaxPages < 1 || cp.MaxPages > 1000 || cp.MaxItems < 1 || cp.MaxItems > 10000 || len(cp.Requests) != len(cp.Bodies) || len(cp.Requests) >= cp.MaxPages || len(cp.Items) >= cp.MaxItems {
		return fail()
	}
	for _, list := range [][]string{cp.Requests, cp.Bodies, cp.Items} {
		seen := map[string]bool{}
		for _, v := range list {
			decoded, err := hex.DecodeString(v)
			if err != nil || len(decoded) != 32 || seen[v] {
				return fail()
			}
			seen[v] = true
		}
	}
	expected, err := f.makeTaskIdentity(task.Stage, req, task.Meta, bindings, task.Meta[ruleAccountMeta], cp)
	if err != nil {
		return req, nil, cp, err
	}
	if expected.URL != task.URL || !bytes.Equal([]byte(expected.Meta[ruleRequestMeta]), []byte(task.Meta[ruleRequestMeta])) {
		return fail()
	}
	return req, bindings, cp, nil
}
