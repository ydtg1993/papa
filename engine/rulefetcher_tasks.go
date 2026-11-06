package engine

import (
	"bytes"
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
	for key := range meta {
		if ruleReserved(key) {
			return nil, fmt.Errorf("rule task: reserved metadata key")
		}
	}
	checkpoint := ruleCheckpoint{MaxPages: f.config.MaxPages, MaxItems: f.config.MaxItems}
	return f.makeTask(f.config.Stage, request, meta, f.config.Bindings, f.config.AccountID, checkpoint)
}

func (f *RuleFetcher) makeTask(stage string, request RuleRequest, meta map[string]string, bindings map[string]RuleRef, account string, checkpoint ruleCheckpoint) (*Task, error) {
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
	if _, err := f.config.Registry.Verified(ref.ID, ref.Version); err != nil {
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
		if stage == "" || ref.ID == "" || ref.Version == "" {
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
	expected, err := f.makeTask(task.Stage, req, task.Meta, bindings, task.Meta[ruleAccountMeta], cp)
	if err != nil {
		return req, nil, cp, err
	}
	if expected.URL != task.URL || !bytes.Equal([]byte(expected.Meta[ruleRequestMeta]), []byte(task.Meta[ruleRequestMeta])) {
		return fail()
	}
	return req, bindings, cp, nil
}
