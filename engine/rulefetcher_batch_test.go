package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
)

func TestRuleBatchPropagationSeparatesRoundsAndPreservesRetry(t *testing.T) {
	cfg := ruleTestConfig(ruleTestRegistry(t, "https://example.com"), "https://example.com")
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	cfg.Bindings["leaf"] = RuleRef{"detail", "v1"}
	f := ruleTestFetcher(t, cfg)
	page := RulePage{SourceURL: "https://example.com/list", Result: crawlrule.Result{Records: []map[string]any{{"id": "1", "url": "/detail"}}}}
	var prior []*Task
	for _, batch := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		root, err := f.makeTask("list", RuleRequest{URL: page.SourceURL}, map[string]string{"_rule_batch": batch}, cfg.Bindings, "", ruleCheckpoint{MaxPages: 10, MaxItems: 10})
		if err != nil {
			t.Fatal(err)
		}
		if root.Meta["_rule_batch"] != batch {
			t.Fatal("batch identity lost while creating root")
		}
		_, bindings, cp, err := f.taskState(root)
		if err != nil {
			t.Fatal(err)
		}
		children, err := f.ruleChildren(context.Background(), root, bindings, cp, page, &RuleRequest{URL: "https://example.com/page2"})
		if err != nil {
			t.Fatal(err)
		}
		retry, err := f.ruleChildren(context.Background(), root, bindings, cp, page, &RuleRequest{URL: "https://example.com/page2"})
		if err != nil || !reflect.DeepEqual(children, retry) {
			t.Fatalf("unstable retry: %v", err)
		}
		for i, child := range children {
			if child.Meta["_rule_batch"] != batch {
				t.Fatalf("child %d lost batch", i)
			}
			if prior != nil && child.URL == prior[i].URL {
				t.Fatalf("child %d reused another round's identity", i)
			}
		}
		detailConfig := cfg
		detailConfig.Stage = "detail"
		detailConfig.Links = []RuleLink{{Field: "url", Stage: "leaf"}}
		detail := ruleTestFetcher(t, detailConfig)
		_, bindings, checkpoint, err := detail.taskState(children[0])
		if err != nil {
			t.Fatal(err)
		}
		leaves, err := detail.ruleChildren(context.Background(), children[0], bindings, checkpoint, page, nil)
		if err != nil || len(leaves) != 1 || leaves[0].Meta[ruleBatchMeta] != batch {
			t.Fatalf("grandchild lost batch: %+v %v", leaves, err)
		}
		prior = children
	}
}

func TestRuleBatchIsVisibleToSaveAndOldTasksRemainUnbatched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"rows":[{"id":"1"}]}`) }))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	var saved string
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { saved = p.BatchID; return nil }
	f := ruleTestFetcher(t, cfg)
	old := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/list"})
	if _, err := ruleTestRun(t, f, old); err != nil || saved != "" {
		t.Fatalf("old task: %q %v", saved, err)
	}
	req, bindings, cp, err := f.taskState(old)
	if err != nil {
		t.Fatal(err)
	}
	batch := strings.Repeat("a", 64)
	root, err := f.makeTask(cfg.Stage, req, map[string]string{ruleBatchMeta: batch}, bindings, "", cp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ruleTestRun(t, f, root); err != nil || saved != batch {
		t.Fatalf("Save batch: %q %v", saved, err)
	}
	for _, bad := range []string{"", strings.Repeat("A", 64), "short"} {
		if _, err = f.makeTask(cfg.Stage, req, map[string]string{ruleBatchMeta: bad}, bindings, "", cp); err == nil {
			t.Fatalf("malformed batch %q accepted", bad)
		}
	}
	root.Meta[ruleBatchMeta] = strings.Repeat("b", 64)
	if _, _, _, err = f.taskState(root); err == nil {
		t.Fatal("tampered persisted batch accepted")
	}
}
