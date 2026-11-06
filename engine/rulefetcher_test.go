package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func ruleTestRegistry(t *testing.T, base string) *crawlrule.Registry {
	t.Helper()
	reg := crawlrule.NewRegistry()
	for _, id := range []string{"list", "detail"} {
		for _, version := range []string{"v1", "v2"} {
			r := crawlrule.Rule{SchemaVersion: 1, ID: id, Version: version, Format: "json", Rows: "/rows", Fields: []crawlrule.Field{{Name: "id", Pointer: "/id", Validation: crawlrule.Validation{Required: true}}, {Name: "url", Pointer: "/url"}, {Name: "cursor", Pointer: "/cursor"}}, Next: &crawlrule.Next{Pointer: "/next"}}
			if err := reg.Register(r); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(base + "/fixture")
			if _, err := reg.Activate(id, version, []crawlrule.Fixture{{Name: "sample", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`{"rows":[{"id":"fixture"}]}`)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/id", Value: "fixture"}}}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return reg
}
func ruleTestConfig(reg *crawlrule.Registry, origin string) RuleFetcherConfig {
	return RuleFetcherConfig{Site: "rules", Stage: "list", Registry: reg, Bindings: map[string]RuleRef{"list": {"list", "v1"}, "detail": {"detail", "v1"}}, Origins: []string{origin}, ItemKey: "id", Save: func(context.Context, *Task, RulePage) error { return nil }}
}
func ruleTestFetcher(t *testing.T, cfg RuleFetcherConfig) *RuleFetcher {
	t.Helper()
	f, err := NewRuleFetcher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func ruleTestTask(t *testing.T, f *RuleFetcher, req RuleRequest) *Task {
	t.Helper()
	task, err := f.NewTask(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func ruleTestRun(t *testing.T, f *RuleFetcher, task *Task) ([]*Task, error) {
	t.Helper()
	var children []*Task
	err := f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{}), func(child *Task) error { children = append(children, child); return nil })
	return children, err
}

func TestRuleFetcherPinsVersionsAndRestoresMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/detail"}],"next":"/page2"}`)
	}))
	defer srv.Close()
	reg := ruleTestRegistry(t, srv.URL)
	cfg := ruleTestConfig(reg, srv.URL)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail", Meta: map[string]string{"parent_id": "id"}}}
	var saved RulePage
	cfg.Save = func(_ context.Context, t *Task, p RulePage) error {
		saved = p
		t.Meta["rule_version"] = "corrupt"
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	seed := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/page1"})
	// A changed startup config and active version cannot rebind a persisted task.
	cfg.Bindings["list"] = RuleRef{"list", "v2"}
	cfg.Bindings["detail"] = RuleRef{"detail", "v2"}
	changed := ruleTestFetcher(t, cfg)
	model := seed.toModel()
	model.ID = 41
	restored := (&Engine{}).taskFromRecord(&model)
	children, err := ruleTestRun(t, changed, restored)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Rule.Version != "v1" || seed.Meta["rule_version"] != "v1" || restored.Meta["rule_version"] != "v1" || len(children) != 2 {
		t.Fatalf("pinning: %+v, children=%d", saved.Rule, len(children))
	}
	for _, child := range children {
		if child.Meta["rule_version"] != "v1" || child.PID != 41 {
			t.Fatalf("child binding: %+v", child)
		}
	}
	if children[0].Meta["parent_id"] != "1" {
		t.Fatal("detail metadata mapping missing")
	}
	newer := ruleTestTask(t, changed, RuleRequest{URL: srv.URL + "/page1"})
	if models.UrlHash(newer.URL) == models.UrlHash(seed.URL) || newer.Unique() == seed.Unique() {
		t.Fatal("different versions collapsed by database or memory identity")
	}
}

func TestRuleFetcherPOSTCursorRetryAndRestart(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Fragment != "" {
			t.Errorf("wire request: %s %s", r.Method, r.URL)
		}
		b, _ := io.ReadAll(r.Body)
		cursors = append(cursors, string(b))
		if string(b) == "first" {
			fmt.Fprint(w, `{"rows":[{"id":"1","cursor":"second"}]}`)
		} else {
			fmt.Fprint(w, `{"rows":[{"id":"2","cursor":""}]}`)
		}
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.ReplaySafe = true
	cfg.BuildRequest = func(_ context.Context, _ *Task, r RuleRequest) (htmlfetch.Request, error) {
		return htmlfetch.Request{URL: r.URL, Method: "POST", Body: []byte(r.State)}, nil
	}
	cfg.Next = func(_ context.Context, _ *Task, p RulePage) (*RuleRequest, error) {
		cursor := p.Result.Records[0]["cursor"].(string)
		if cursor == "" {
			return nil, nil
		}
		return &RuleRequest{URL: p.SourceURL, State: cursor}, nil
	}
	saves := 0
	stored := map[string]bool{}
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
		saves++
		if saves == 1 {
			return errors.New("store unavailable")
		}
		for _, r := range p.Result.Records {
			stored[r["id"].(string)] = true
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	seed := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/api", State: "first"})
	before := ruleJSON(seed.Meta)
	children, err := ruleTestRun(t, f, seed)
	if err == nil || !core.Retryable(err) || len(children) != 0 || before != ruleJSON(seed.Meta) {
		t.Fatalf("failed save advanced checkpoint: %v", err)
	}
	children, err = ruleTestRun(t, f, seed)
	if err != nil || len(children) != 1 {
		t.Fatalf("first page: %v children=%d", err, len(children))
	}
	second := children[0]
	if models.UrlHash(second.URL) == models.UrlHash(seed.URL) {
		t.Fatal("POST cursors collapsed at URL unique index")
	}
	record := second.toModel()
	record.ID = 9
	restored := (&Engine{}).taskFromRecord(&record)
	final, err := ruleTestRun(t, f, restored)
	if err != nil || len(final) != 0 || len(stored) != 2 {
		t.Fatalf("restored cursor: %v data=%v", err, stored)
	}
	if !reflect.DeepEqual(cursors, []string{"first", "first", "second"}) {
		t.Fatalf("request replay: %v", cursors)
	}
	if before != ruleJSON(seed.Meta) {
		t.Fatal("parent task checkpoint mutated")
	}
}

func TestRuleFetcherPartialSubmitReplaysIdempotentSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/a"},{"id":"2","url":"/b"}]}`)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	saved := map[string]bool{}
	saveCalls := 0
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
		saveCalls++
		for _, r := range p.Result.Records {
			saved[r["id"].(string)] = true
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	accepted := map[string]bool{}
	firstURL := ""
	calls := 0
	submit := func(child *Task) error {
		calls++
		if calls == 2 {
			return errors.New("queue unavailable")
		}
		accepted[child.Unique()] = true
		if firstURL == "" {
			firstURL = child.URL
		}
		return nil
	}
	client := htmlfetch.NewClient(htmlfetch.Config{})
	if err := f.RunHTTP(context.Background(), task, client, submit); err == nil || !core.Retryable(err) {
		t.Fatalf("submit failure: %v", err)
	}
	if err := f.RunHTTP(context.Background(), task, client, submit); err != nil {
		t.Fatal(err)
	}
	if saveCalls != 2 || len(saved) != 2 || len(accepted) != 2 || !accepted[task.Site+":detail:"+firstURL] {
		t.Fatalf("replay result saves=%d data=%d tasks=%d", saveCalls, len(saved), len(accepted))
	}
}

func TestRuleFetcherPaginationStopsAndFiltersEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, second                             string
		maxPages, maxItems, wantPages, wantItems int
		reason                                   string
	}{
		{"item-limit", `{"rows":[{"id":"1"},{"id":"2"},{"id":"3"}],"next":"/three"}`, 100, 2, 2, 2, "item-limit"},
		{"page-limit", `{"rows":[{"id":"2"}],"next":"/three"}`, 1, 10, 1, 1, "page-limit"},
		{"cycle", `{"rows":[{"id":"2"}],"next":"/one"}`, 100, 10, 2, 2, "request-cycle"},
		{"no-progress", `{"rows":[{"id":"1"}],"next":"/three"}`, 100, 10, 1, 1, ""},
		{"repeated-body", `{"rows":[{"id":"1"}]}`, 100, 10, 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if tc.name == "repeated-body" {
					fmt.Fprint(w, tc.second)
				} else if r.URL.Path == "/one" {
					fmt.Fprint(w, `{"rows":[{"id":"1"}],"next":"/two"}`)
				} else {
					fmt.Fprint(w, tc.second)
				}
			}))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			cfg.MaxPages = tc.maxPages
			cfg.MaxItems = tc.maxItems
			if tc.name == "repeated-body" {
				cfg.Next = func(context.Context, *Task, RulePage) (*RuleRequest, error) {
					return &RuleRequest{URL: srv.URL + "/two"}, nil
				}
			}
			pages, items := 0, 0
			reason := ""
			cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
				pages++
				items += len(p.Result.Records)
				reason = p.StopReason
				if p.Result.Validation.Rows != len(p.Result.Records) {
					t.Error("validation row count stale")
				}
				for _, ev := range p.Result.Evidence {
					if ev.Record >= len(p.Result.Records) {
						t.Error("evidence index stale")
					}
				}
				return nil
			}
			f := ruleTestFetcher(t, cfg)
			queue := []*Task{ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/one"})}
			for len(queue) > 0 {
				task := queue[0]
				queue = queue[1:]
				children, err := ruleTestRun(t, f, task)
				if err != nil {
					t.Fatal(err)
				}
				queue = append(queue, children...)
				if hits > 3 {
					t.Fatal("pagination did not stop")
				}
			}
			if pages != tc.wantPages || items != tc.wantItems || reason != tc.reason {
				t.Fatalf("pages/items/reason=%d/%d/%s", pages, items, reason)
			}
		})
	}
}

func TestRuleFetcherRejectsMalformedTasksAndUnsafePlansBeforeSave(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"https://outside.invalid/secret"}]}`)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	saves := 0
	cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
	f := ruleTestFetcher(t, cfg)
	for _, change := range []func(*Task){func(t *Task) { delete(t.Meta, "rule_version") }, func(t *Task) { t.Meta[ruleCheckpointMeta] = "null" }, func(t *Task) { t.Meta[ruleBindingsMeta] = "null" }, func(t *Task) { t.Meta[ruleRequestMeta] = `{"url":"https://outside.invalid/"}` }, func(t *Task) { t.URL += "tampered" }} {
		task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
		change(task)
		if _, err := ruleTestRun(t, f, task); err == nil || core.Retryable(err) {
			t.Fatalf("malformed task: %v", err)
		}
	}
	if hits != 0 {
		t.Fatal("malformed task reached network")
	}
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	f = ruleTestFetcher(t, cfg)
	if _, err := ruleTestRun(t, f, ruleTestTask(t, f, RuleRequest{URL: srv.URL})); err == nil || core.Retryable(err) {
		t.Fatalf("external child: %v", err)
	}
	if saves != 0 {
		t.Fatal("invalid child plan saved business data")
	}
	if _, err := f.NewTask(RuleRequest{URL: srv.URL}, map[string]string{"rule_version": "bad"}); err == nil {
		t.Fatal("reserved metadata accepted")
	}
}

func TestRuleFetcherEngineQueueEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list":
			fmt.Fprint(w, `{"rows":[{"id":"1","url":"/detail"}],"next":"/list2"}`)
		case "/list2":
			fmt.Fprint(w, `{"rows":[{"id":"2","url":"/detail2"}]}`)
		default:
			fmt.Fprintf(w, `{"rows":[{"id":%q}]}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	var mu sync.Mutex
	saved := map[string]RulePage{}
	done := make(chan struct{}, 4)
	cfg.Save = func(_ context.Context, task *Task, p RulePage) error {
		mu.Lock()
		saved[task.Stage+":"+p.SourceURL] = p
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	list := ruleTestFetcher(t, cfg)
	cfg.Stage = "detail"
	cfg.Links = nil
	detail := ruleTestFetcher(t, cfg)
	fake := newFakeTaskDB()
	fake.noRows = true
	e := newTestEngine(t, fake)
	e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{})
	stage := StageConfig{Site: "rules", WorkerCount: 1, QueueSize: 8, MaxAttempts: 1}
	e.AddStage("list", stage, list, nil)
	e.AddStage("detail", stage, detail, nil)
	e.ApplyRegisterStage()
	defer e.Stop(3 * time.Second)
	if err := e.SubmitTask(ruleTestTask(t, list, RuleRequest{URL: srv.URL + "/list"})); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("engine list/detail/pagination flow did not finish")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, _, p1, q1 := e.pools.get("rules:list").Stats()
		_, _, _, p2, q2 := e.pools.get("rules:detail").Stats()
		if int64(q1+q2)+p1+p2 == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker terminal persistence did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if drained, stats := e.Stop(3 * time.Second); !drained {
		t.Fatalf("engine failed to drain: %+v", stats)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 4 {
		t.Fatalf("pages saved=%d", len(saved))
	}
	for _, page := range saved {
		if page.Rule.Version != "v1" || len(page.BodySHA256) != 64 || len(page.Result.Evidence) == 0 {
			t.Fatal("provenance missing")
		}
	}
	if sql := fake.written(); !strings.Contains(sql, "INSERT INTO `crawler_tasks`") || !strings.Contains(sql, "UPDATE `crawler_tasks`") {
		t.Fatal("queue did not use persistence")
	}
}

func TestRuleTaskIdentitySurvivesModelJSON(t *testing.T) {
	cfg := ruleTestConfig(ruleTestRegistry(t, "https://example.test"), "https://example.test")
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: "https://example.test/api", State: "游标<&>"})
	task.ID = 73
	b, err := json.Marshal(task.toModel())
	if err != nil {
		t.Fatal(err)
	}
	var record models.CrawlerTask
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
	restored := (&Engine{}).taskFromRecord(&record)
	if _, _, _, err := f.taskState(restored); err != nil {
		t.Fatal(err)
	}
	if task.Unique() != restored.Unique() {
		t.Fatal("identity changed across JSON persistence")
	}
}

func TestRuleFetcherNextCannotMutateValidatedPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":{"nested":["original"]},"url":"/detail"}]}`)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	cfg.Next = func(_ context.Context, task *Task, page RulePage) (*RuleRequest, error) {
		task.Meta["rule_version"] = "changed"
		page.Result.Records[0]["id"].(map[string]any)["nested"].([]any)[0] = "changed"
		page.Result.Records[0]["url"] = "https://outside.invalid/"
		page.Result.Evidence[0].Field = "changed"
		return nil, nil
	}
	cfg.Save = func(_ context.Context, _ *Task, page RulePage) error {
		if page.Result.Records[0]["id"].(map[string]any)["nested"].([]any)[0] != "original" || page.Result.Evidence[0].Field == "changed" {
			t.Fatal("planning mutated validated page")
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	children, err := ruleTestRun(t, f, ruleTestTask(t, f, RuleRequest{URL: srv.URL}))
	if err != nil || len(children) != 1 {
		t.Fatalf("planning snapshot: %v children=%d", err, len(children))
	}
}

func TestRuleFetcherNextPreservesRetryClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"rows":[{"id":"1"}]}`) }))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	for _, cause := range []error{context.DeadlineExceeded, core.WrapNoRetry(errors.New("permanent"))} {
		cfg.Next = func(context.Context, *Task, RulePage) (*RuleRequest, error) { return nil, cause }
		f := ruleTestFetcher(t, cfg)
		_, err := ruleTestRun(t, f, ruleTestTask(t, f, RuleRequest{URL: srv.URL}))
		if !errors.Is(err, cause) || core.Retryable(err) != core.Retryable(cause) {
			t.Fatalf("classification changed: %v", err)
		}
	}
}

func TestRuleFetcherChildBudgetFailsBeforeSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/a"},{"id":"2","url":"/b"}]}`)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	cfg.MaxChildren = 1
	saves := 0
	cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
	f := ruleTestFetcher(t, cfg)
	children, err := ruleTestRun(t, f, ruleTestTask(t, f, RuleRequest{URL: srv.URL}))
	if err == nil || core.Retryable(err) || saves != 0 || len(children) != 0 {
		t.Fatalf("child budget allowed partial side effects: err=%v saves=%d children=%d", err, saves, len(children))
	}
}

func TestRuleFetcherRejectsUnpersistableIdentity(t *testing.T) {
	cfg := ruleTestConfig(ruleTestRegistry(t, "https://example.test"), "https://example.test")
	f := ruleTestFetcher(t, cfg)
	if _, err := f.NewTask(RuleRequest{URL: "https://example.test/?q=" + strings.Repeat("a", 65535)}, nil); err == nil {
		t.Fatal("URL cannot fit MySQL TEXT but was accepted")
	}
	cfg.Site = strings.Repeat("x", 129)
	if _, err := NewRuleFetcher(cfg); err == nil {
		t.Fatal("site cannot fit database column but was accepted")
	}
}
