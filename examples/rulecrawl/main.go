// rulecrawl runs fixture-verified extraction against its own loopback server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func main() {
	mode := flag.String("mode", "html", "catalog transport: html or json (replay-safe cursor POST)")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout, *mode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type businessRecord struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Rule        engine.RuleRef `json:"rule"`
	SourcePath  string         `json:"source_path"`
	BodySHA256  string         `json:"body_sha256"`
}

type acceptedPage struct {
	Stage      string         `json:"stage"`
	Number     int            `json:"number"`
	SourcePath string         `json:"source_path"`
	Rule       engine.RuleRef `json:"rule"`
	Items      int            `json:"accepted_items"`
	StopReason string         `json:"stop_reason,omitempty"`
}

type report struct {
	Mode          string           `json:"mode"`
	CatalogPages  int              `json:"catalog_pages"`
	CatalogItems  int              `json:"catalog_items"`
	DetailPages   int              `json:"detail_pages"`
	ReplayChecked bool             `json:"detail_replay_checked"`
	Records       []businessRecord `json:"records"`
	Pages         []acceptedPage   `json:"pages"`
}

// The example is single-threaded. Production Save must use database uniqueness
// and an upsert/transaction so concurrent workers and retries are idempotent.
type memoryStore struct {
	records map[string]businessRecord
	pages   map[string]acceptedPage
}

func (s *memoryStore) save(_ context.Context, task *engine.Task, page engine.RulePage) error {
	u, err := url.Parse(page.SourceURL)
	if err != nil {
		return err
	}
	path := u.RequestURI() // Keep output deterministic despite the random local port.
	key := fmt.Sprintf("%s:%d:%s", task.Stage, page.Page, path)
	s.pages[key] = acceptedPage{Stage: task.Stage, Number: page.Page, SourcePath: path,
		Rule: page.Rule, Items: len(page.Result.Records), StopReason: page.StopReason}
	if task.Stage == "detail" {
		for _, row := range page.Result.Records {
			id := row["id"].(string) // Required string fields were validated by the rule.
			s.records[id] = businessRecord{ID: id, Title: row["title"].(string),
				Description: row["description"].(string), Rule: page.Rule,
				SourcePath: path, BodySHA256: page.BodySHA256}
		}
	}
	return nil
}

func run(ctx context.Context, output io.Writer, mode string) error {
	if mode != "html" && mode != "json" {
		return fmt.Errorf("mode must be html or json")
	}
	server := httptest.NewServer(http.HandlerFunc(serveDemo))
	defer server.Close()
	registry, bindings, err := verifiedRules(mode)
	if err != nil {
		return err
	}
	store := &memoryStore{records: make(map[string]businessRecord), pages: make(map[string]acceptedPage)}
	base := engine.RuleFetcherConfig{
		Site: "demo", Registry: registry, Bindings: bindings, Origins: []string{server.URL},
		Transport: "http", MaxPages: 4, MaxItems: 8, ItemKey: "id", Save: store.save,
	}
	detailConfig := base
	detailConfig.Stage = "detail"
	detail, err := engine.NewRuleFetcher(detailConfig)
	if err != nil {
		return err
	}
	listConfig := base
	listConfig.Stage = "catalog"
	listConfig.Links = []engine.RuleLink{{Field: "href", Stage: "detail", Meta: map[string]string{"item_id": "id"}}}
	entryURL := server.URL + "/list1"
	if mode == "json" {
		entryURL = server.URL + "/api/catalog"
		listConfig.ReplaySafe = true // This local POST only reads data.
		listConfig.BuildRequest = func(_ context.Context, _ *engine.Task, req engine.RuleRequest) (htmlfetch.Request, error) {
			body, err := json.Marshal(map[string]string{"cursor": req.State})
			return htmlfetch.Request{URL: req.URL, Method: http.MethodPost,
				Headers: map[string]string{"Content-Type": "application/json"}, Body: body}, err
		}
	}
	list, err := engine.NewRuleFetcher(listConfig)
	if err != nil {
		return err
	}
	entry, err := list.NewTask(engine.RuleRequest{URL: entryURL}, nil)
	if err != nil {
		return err
	}

	// This queue is deliberately process-local. Production FetchHandler submits
	// through Engine.SubmitTask and the normal persistent crawler task lifecycle.
	const maxTasks = 16
	queue := make([]*engine.Task, 0, maxTasks)
	seen := make(map[string]bool)
	submit := func(task *engine.Task) error {
		if seen[task.Unique()] {
			return nil
		}
		if len(seen) >= maxTasks {
			return fmt.Errorf("demo task budget exceeded")
		}
		seen[task.Unique()] = true
		task.ID = len(seen)
		queue = append(queue, task)
		return nil
	}
	if err := submit(entry); err != nil {
		return err
	}
	client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{server.URL}}, Timeout: 2 * time.Second, MaxBodySize: 64 << 10})
	fetchers := map[string]*engine.RuleFetcher{"catalog": list, "detail": detail}
	var replay *engine.Task
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		task := queue[0]
		queue = queue[1:]
		if err := fetchers[task.Stage].RunHTTP(ctx, task, client, submit); err != nil {
			return err
		}
		if task.Stage == "detail" && replay == nil {
			replay = task
		}
	}
	// Simulate a worker retry after a successful Save. Upserts keep one record.
	if replay == nil {
		return fmt.Errorf("demo produced no detail tasks")
	}
	if err := detail.RunHTTP(ctx, replay, client, submit); err != nil {
		return err
	}
	result := report{Mode: mode, ReplayChecked: true}
	for _, record := range store.records {
		result.Records = append(result.Records, record)
	}
	for _, page := range store.pages {
		result.Pages = append(result.Pages, page)
		if page.Stage == "catalog" {
			result.CatalogPages++
			result.CatalogItems += page.Items
		} else {
			result.DetailPages++
		}
	}
	sort.Slice(result.Records, func(i, j int) bool { return result.Records[i].ID < result.Records[j].ID })
	sort.Slice(result.Pages, func(i, j int) bool {
		a, b := result.Pages[i], result.Pages[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.Number != b.Number {
			return a.Number < b.Number
		}
		return a.SourcePath < b.SourcePath
	})
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

const (
	list1   = `<article data-id="A"><h2>Alpha</h2><a href="/detail/A">detail</a></article><a rel="next" href="/list2">next</a>`
	list2   = `<article data-id="A"><h2>Alpha</h2><a href="/detail/A">detail</a></article><article data-id="B"><h2>Beta</h2><a href="/detail/B">detail</a></article>`
	detailA = `<article data-id="A"><h1>Alpha</h1><p>First example record.</p></article>`
	detailB = `<article data-id="B"><h1>Beta</h1><p>Second example record.</p></article>`
	json1   = `{"items":[{"id":"A","title":"Alpha","href":"/detail/A"}],"next_cursor":"page2"}`
	json2   = `{"items":[{"id":"A","title":"Alpha","href":"/detail/A"},{"id":"B","title":"Beta","href":"/detail/B"}],"next_cursor":""}`
)

func serveDemo(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/catalog" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			Cursor string `json:"cursor"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&request); err != nil {
			http.Error(w, "invalid cursor request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Cursor {
		case "":
			io.WriteString(w, json1)
		case "page2":
			io.WriteString(w, json2)
		default:
			http.Error(w, "unknown cursor", http.StatusBadRequest)
		}
		return
	}
	pages := map[string]string{"/list1": list1, "/list2": list2, "/detail/A": detailA, "/detail/B": detailB}
	if body, ok := pages[r.URL.Path]; ok && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
		return
	}
	http.NotFound(w, r)
}

// verifiedRules activates explicit saved samples before creating any task.
func verifiedRules(mode string) (*crawlrule.Registry, map[string]engine.RuleRef, error) {
	registry := crawlrule.NewRegistry()
	text := crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}
	catalog := crawlrule.Rule{SchemaVersion: 1, ID: "demo-catalog", Version: mode + "-v1", Format: mode,
		Rows: "article", MaxRows: 8, MaxBodyBytes: 64 << 10,
		Fields: []crawlrule.Field{
			{Name: "id", Attribute: "data-id", Validation: text},
			{Name: "title", Selector: "h2", Validation: text},
			{Name: "href", Selector: "a", Attribute: "href", Validation: text},
		}, Next: &crawlrule.Next{Selector: "a[rel=next]"},
	}
	catalogFixture := sample("catalog-page-1", "/list1", list1, "Alpha", "https://fixtures.example/list2")
	if mode == "json" {
		catalog.Rows = "/items"
		catalog.Next = &crawlrule.Next{Mode: "cursor", Pointer: "/next_cursor"}
		catalog.Fields = []crawlrule.Field{
			{Name: "id", Pointer: "/id", Validation: text},
			{Name: "title", Pointer: "/title", Validation: text},
			{Name: "href", Pointer: "/href", Validation: text},
		}
		catalogFixture = sample("catalog-cursor-1", "/api/catalog", json1, "Alpha", "")
		catalogFixture.Expect.NextCursor = "page2"
	}
	detail := crawlrule.Rule{SchemaVersion: 1, ID: "demo-detail", Version: "html-v1", Format: "html",
		Rows: "article", MaxRows: 1, MaxBodyBytes: 64 << 10,
		Fields: []crawlrule.Field{
			{Name: "id", Attribute: "data-id", Validation: text},
			{Name: "title", Selector: "h1", Validation: text},
			{Name: "description", Selector: "p", Validation: text},
		},
	}
	detailFixture := sample("detail-A", "/detail/A", detailA, "Alpha", "")
	detailFixture.Expect.Fields = append(detailFixture.Expect.Fields, crawlrule.FieldExpectation{Pointer: "/description", Value: "First example record."})
	for _, entry := range []struct {
		rule    crawlrule.Rule
		fixture crawlrule.Fixture
	}{{catalog, catalogFixture}, {detail, detailFixture}} {
		if err := registry.Register(entry.rule); err != nil {
			return nil, nil, err
		}
		if _, err := registry.Activate(entry.rule.ID, entry.rule.Version, []crawlrule.Fixture{entry.fixture}); err != nil {
			return nil, nil, err
		}
	}
	return registry, map[string]engine.RuleRef{
		"catalog": {ID: catalog.ID, Version: catalog.Version},
		"detail":  {ID: detail.ID, Version: detail.Version},
	}, nil
}

func sample(name, path, body, title, next string) crawlrule.Fixture {
	u, _ := url.Parse("https://fixtures.example" + path)
	return crawlrule.Fixture{Name: name, Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(body)},
		Expect: crawlrule.Expectations{RowCount: 1, NextURL: next,
			Fields: []crawlrule.FieldExpectation{{Pointer: "/id", Value: "A"}, {Pointer: "/title", Value: title}}}}
}
