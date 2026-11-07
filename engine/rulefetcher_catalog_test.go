package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

type testRuleCatalog struct {
	mu        sync.Mutex
	registry  *crawlrule.Registry
	snapshot  crawlrule.ReleaseSnapshot
	snapshots int
	err       error
}

func (c *testRuleCatalog) Resolve(ctx context.Context, id, version string) (*crawlrule.Compiled, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return c.registry.Verified(id, version)
}
func (c *testRuleCatalog) Snapshot(ctx context.Context, site string) (crawlrule.ReleaseSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return crawlrule.ReleaseSnapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshots++
	if c.err != nil {
		return crawlrule.ReleaseSnapshot{}, c.err
	}
	s := c.snapshot
	s.Bindings = maps.Clone(s.Bindings)
	return s, nil
}
func (c *testRuleCatalog) publish(version string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = crawlrule.ReleaseSnapshot{ID: fmt.Sprintf("release-%d", generation), Site: "rules", Generation: generation, Bindings: map[string]crawlrule.Reference{"list": {ID: "list", Version: version}, "detail": {ID: "detail", Version: version}}}
}

func TestRuleCatalogPublicationPinsWholeWorkflowAndRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/detail"}],"next":"/page2"}`)
	}))
	defer srv.Close()
	reg := ruleTestRegistry(t, srv.URL)
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, srv.URL)
	cfg.Registry = nil
	cfg.Bindings = nil
	cfg.Catalog = catalog
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	var saved RulePage
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { saved = p; return nil }
	f := ruleTestFetcher(t, cfg)
	if catalog.snapshots != 0 {
		t.Fatal("constructor performed a publication read")
	}
	old, err := f.NewTaskContext(context.Background(), RuleRequest{URL: srv.URL + "/page1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog.publish("v2", 2)
	newer, err := f.NewTaskContext(context.Background(), RuleRequest{URL: srv.URL + "/page1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.URL == newer.URL || old.Meta["rule_version"] != "v1" || newer.Meta["rule_version"] != "v2" {
		t.Fatal("publication did not pin independent task identity")
	}
	// Reconstruct the handler and task as after a process restart. Execution must
	// never consult the current binding pointer, even if it has moved again.
	catalog.publish("v1", 3)
	f = ruleTestFetcher(t, cfg)
	m := old.toModel()
	m.ID = 17
	restored := (&Engine{}).taskFromRecord(&m)
	children, err := ruleTestRun(t, f, restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || saved.Rule.Version != "v1" || saved.ReleaseID != "release-1" || saved.ReleaseGeneration != 1 {
		t.Fatalf("pinned result %+v, children %d", saved, len(children))
	}
	for _, child := range children {
		if child.Meta[ruleReleaseMeta] != old.Meta[ruleReleaseMeta] || child.Meta["rule_version"] != "v1" || child.Meta[ruleBindingsMeta] != old.Meta[ruleBindingsMeta] {
			t.Fatalf("child lost release binding: %+v", child)
		}
	}
	if catalog.snapshots != 2 {
		t.Fatalf("worker reread current publication: reads=%d", catalog.snapshots)
	}
}

func TestRuleCatalogValidationAndReadFailures(t *testing.T) {
	reg := ruleTestRegistry(t, "https://example.test")
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, "https://example.test")
	cfg.Catalog = catalog
	if _, err := NewRuleFetcher(cfg); err == nil {
		t.Fatal("mixed catalogs accepted")
	}
	cfg.Registry = nil
	if _, err := NewRuleFetcher(cfg); err == nil {
		t.Fatal("static bindings mixed with publications")
	}
	cfg.Bindings = nil
	f := ruleTestFetcher(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.NewTaskContext(ctx, RuleRequest{URL: "https://example.test/"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, mutate := range []func(*crawlrule.ReleaseSnapshot){
		func(s *crawlrule.ReleaseSnapshot) { s.Site = "other" },
		func(s *crawlrule.ReleaseSnapshot) { s.ID = "" },
		func(s *crawlrule.ReleaseSnapshot) { s.Generation = 0 },
		func(s *crawlrule.ReleaseSnapshot) { delete(s.Bindings, "list") },
		func(s *crawlrule.ReleaseSnapshot) {
			s.Bindings["list"] = crawlrule.Reference{ID: "list", Version: "unpublished"}
		},
	} {
		catalog.publish("v1", 1)
		mutate(&catalog.snapshot)
		if _, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/"}, nil); err == nil {
			t.Fatal("malformed publication accepted")
		}
	}
	catalog.publish("v1", 1)
	task, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog.err = errors.New("temporary catalog database failure")
	err = f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{}), func(*Task) error { return nil })
	if !errors.Is(err, catalog.err) || !core.Retryable(err) {
		t.Fatalf("catalog outage must remain retryable: %v", err)
	}
	catalog.err = crawlrule.ErrVersionNotFound
	err = f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{}), func(*Task) error { return nil })
	if !errors.Is(err, crawlrule.ErrVersionNotFound) || core.Retryable(err) {
		t.Fatalf("missing pinned version: %v", err)
	}
}

func TestRuleCatalogLegacyTasksAndConcurrentPublication(t *testing.T) {
	reg := ruleTestRegistry(t, "https://example.test")
	legacy := ruleTestFetcher(t, ruleTestConfig(reg, "https://example.test"))
	old := ruleTestTask(t, legacy, RuleRequest{URL: "https://example.test/page"})
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, "https://example.test")
	cfg.Registry = nil
	cfg.Bindings = nil
	cfg.Catalog = catalog
	f := ruleTestFetcher(t, cfg)
	if _, _, _, err := f.taskState(old); err != nil {
		t.Fatalf("legacy task metadata no longer accepted: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			catalog.publish(fmt.Sprintf("v%d", i%2+1), uint64(i+2))
			task, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/page"}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, bindings, _, err := f.taskState(task)
			if err != nil || bindings["list"].Version != bindings["detail"].Version {
				t.Errorf("mixed release: %+v, %v", bindings, err)
			}
		}(i)
	}
	wg.Wait()
	catalog.publish("v1", 50)
	a, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/page"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog.publish("v1", 51)
	b, err := f.NewTaskContext(context.Background(), RuleRequest{URL: "https://example.test/page"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.URL == b.URL {
		t.Fatal("separate publications with identical rules collapsed")
	}
	a.Meta[ruleReleaseMeta] = b.Meta[ruleReleaseMeta]
	if _, _, _, err := f.taskState(a); err == nil {
		t.Fatal("tampered publication marker did not invalidate task identity")
	}
}

func TestRuleCatalogPublicationIdentityReachesEvidenceAndTrace(t *testing.T) {
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	e, trace, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	reg := ruleTestRegistry(t, srv.URL)
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 7)
	cfg := ruleTestConfig(reg, srv.URL)
	cfg.Registry = nil
	cfg.Bindings = nil
	cfg.Catalog = catalog
	cfg.SnapshotPolicy = snapshotPolicy()
	boom := errors.New("save failed")
	var saved RulePage
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { saved = p; return boom }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 81
	if err := e.runAttempt(context.Background(), f, task, 0); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	observations := readRuleObservations(t, dir)
	if len(observations) != 1 {
		t.Fatalf("observations: %d", len(observations))
	}
	m := observations[0]
	if m.ReleaseID != "release-7" || m.ReleaseGeneration != 7 || m.ReleaseID != saved.ReleaseID || m.EvidenceID != saved.EvidenceID || !strings.Contains(trace.all(), m.ReleaseID) {
		t.Fatalf("lost publication provenance: %+v", m)
	}
}

func TestRuleCatalogRunsMigratedLegacyTaskWithoutRebinding(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"rows":[{"id":"old"}]}`)
	}))
	defer srv.Close()
	reg := ruleTestRegistry(t, srv.URL)
	static := ruleTestFetcher(t, ruleTestConfig(reg, srv.URL))
	task := ruleTestTask(t, static, RuleRequest{URL: srv.URL})
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v2", 2)
	cfg := ruleTestConfig(reg, srv.URL)
	cfg.Registry = nil
	cfg.Bindings = nil
	cfg.Catalog = catalog
	saves := 0
	cfg.Save = func(_ context.Context, _ *Task, page RulePage) error {
		saves++
		if page.Rule.Version != "v1" || page.ReleaseID != "" || page.ReleaseGeneration != 0 {
			t.Errorf("legacy version rewritten: %+v", page)
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	if _, err := ruleTestRun(t, f, task); err != nil {
		t.Fatal(err)
	}
	if saves != 1 || requests.Load() != 1 || catalog.snapshots != 0 {
		t.Fatal("legacy execution did not retain pinned path")
	}
	// A migration must import and publish every still-referenced old version.
	// The framework cannot silently substitute the current release when absent.
	catalog.err = crawlrule.ErrVersionNotFound
	if _, err := ruleTestRun(t, f, task); !errors.Is(err, crawlrule.ErrVersionNotFound) || core.Retryable(err) {
		t.Fatalf("missing migration: %v", err)
	}
	if saves != 1 || requests.Load() != 1 {
		t.Fatal("missing old version performed network/business work")
	}
}
