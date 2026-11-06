// ruleperiodic runs two complete RuleFetcher batches through real Engine workers
// and repeat_queue, using only fixture-authored loopback pages.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const interval = 10 * time.Second

type options struct{ Migrate bool }

func main() {
	var opts options
	flag.BoolVar(&opts.Migrate, "migrate", false, "explicitly migrate the dedicated rule_test database before running")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := command(ctx, os.Stdout, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, output io.Writer, opts options) error {
	dsn := os.Getenv("PAPA_RULE_SCHEDULE_DSN")
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || dsn == "" || !strings.Contains(parsed.DBName, "rule_test") {
		return errors.New("ruleperiodic: PAPA_RULE_SCHEDULE_DSN must name a dedicated MySQL database containing rule_test")
	}
	result, err := run(ctx, dsn, opts)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func openDatabase(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, errors.New("ruleperiodic: cannot connect to the dedicated example database")
	}
	return db, nil
}

func exampleConfig() *config.Config {
	return &config.Config{
		Rules:       config.RulesConfig{Enabled: true, SampleDir: "./ruleperiodic-samples"},
		HTML:        config.HTMLConfig{Enable: true, Timeout: 3 * time.Second, MaxBodySize: 64 << 10},
		Crawler:     config.CrawlerConfig{DrainInterval: 10 * time.Millisecond, DedupCacheSize: 1024},
		RepeatQueue: config.RepeatQueueConfig{Enabled: true, WorkerCount: 1, BatchSize: 16, Interval: interval},
	}
}

type businessRecord struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	BatchID string `json:"batch_id"`
}

type batchResult struct {
	Status  engine.RuleScheduleStatus `json:"status"`
	TaskIDs []uint                    `json:"task_ids"`
	Records []businessRecord          `json:"records"`
}

type demoResult struct {
	Site           string                    `json:"site"`
	ControlID      uint                      `json:"control_id"`
	IntervalSecond int                       `json:"interval_seconds"`
	Batches        []batchResult             `json:"batches"`
	Requests       map[string]int            `json:"requests"`
	ControlRepeat  int                       `json:"control_repeat"`
	Reopened       engine.RuleScheduleStatus `json:"reopened_status"`
	ReopenedEqual  bool                      `json:"reopened_equal"`
}

// The example business store is in memory; task and schedule state use MySQL.
// Production Save needs a business unique key and a durable upsert/transaction.
type businessStore struct {
	mu      sync.Mutex
	records map[string]businessRecord
}

func (s *businessStore) save(_ context.Context, task *engine.Task, page engine.RulePage) error {
	if task.Stage != "detail" {
		return nil
	}
	if page.BatchID == "" {
		return errors.New("example detail page has no batch identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range page.Result.Records {
		id, title := row["id"].(string), row["title"].(string)
		s.records[id] = businessRecord{ID: id, Title: title, BatchID: page.BatchID}
	}
	return nil
}

func (s *businessStore) snapshot() []businessRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]businessRecord, 0, len(s.records))
	for _, row := range s.records {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

type localSite struct {
	*httptest.Server
	version atomic.Int64
	mu      sync.Mutex
	hits    map[string]int
}

func newLocalSite() *localSite {
	s := &localSite{hits: make(map[string]int)}
	s.version.Store(1)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := pageBody(r.URL.Path, int(s.version.Load()))
		if r.Method != http.MethodGet || body == "" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	return s
}

func (s *localSite) requests() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]int, len(s.hits))
	for path, n := range s.hits {
		result[path] = n
	}
	return result
}

func pageBody(path string, version int) string {
	switch path {
	case "/list1":
		return `<article data-id="A"><a href="/detail/A">detail</a></article><a rel="next" href="/list2">next</a>`
	case "/list2":
		return `<article data-id="B"><a href="/detail/B">detail</a></article>`
	case "/detail/A":
		return fmt.Sprintf(`<article data-id="A"><h1>Alpha-v%d</h1></article>`, version)
	case "/detail/B":
		return fmt.Sprintf(`<article data-id="B"><h1>Beta-v%d</h1></article>`, version)
	default:
		return ""
	}
}

func verifiedRules() (*crawlrule.Registry, map[string]engine.RuleRef, error) {
	registry := crawlrule.NewRegistry()
	text := crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}
	rules := []crawlrule.Rule{
		{SchemaVersion: 1, ID: "periodic-catalog", Version: "v1", Format: "html", Rows: "article", MaxRows: 4, MaxBodyBytes: 64 << 10,
			Fields: []crawlrule.Field{{Name: "id", Attribute: "data-id", Validation: text}, {Name: "href", Selector: "a", Attribute: "href", Validation: text}},
			Next:   &crawlrule.Next{Selector: "a[rel=next]"}},
		{SchemaVersion: 1, ID: "periodic-detail", Version: "v1", Format: "html", Rows: "article", MaxRows: 1, MaxBodyBytes: 64 << 10,
			Fields: []crawlrule.Field{{Name: "id", Attribute: "data-id", Validation: text}, {Name: "title", Selector: "h1", Validation: text}}},
	}
	bindings := make(map[string]engine.RuleRef)
	for i, rule := range rules {
		stage := []string{"catalog", "detail"}[i]
		var fixtures []crawlrule.Fixture
		for _, id := range []string{"A", "B"} {
			path := "/detail/" + id
			if stage == "catalog" {
				path = map[string]string{"A": "/list1", "B": "/list2"}[id]
			}
			for version := 1; version <= 2; version++ {
				u, _ := url.Parse("https://fixtures.example" + path)
				expect := crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/id", Value: id}}}
				if stage == "catalog" {
					expect.Fields = append(expect.Fields, crawlrule.FieldExpectation{Pointer: "/href", Value: "/detail/" + id})
					if id == "A" {
						expect.NextURL = "https://fixtures.example/list2"
					}
				} else {
					title := fmt.Sprintf("%s-v%d", map[string]string{"A": "Alpha", "B": "Beta"}[id], version)
					expect.Fields = append(expect.Fields, crawlrule.FieldExpectation{Pointer: "/title", Value: title})
				}
				fixtures = append(fixtures, crawlrule.Fixture{Name: fmt.Sprintf("%s-%s-v%d", stage, id, version), Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(pageBody(path, version))}, Expect: expect})
			}
		}
		if err := registry.Register(rule); err != nil {
			return nil, nil, err
		}
		if _, err := registry.Activate(rule.ID, rule.Version, fixtures); err != nil {
			return nil, nil, err
		}
		bindings[stage] = engine.RuleRef{ID: rule.ID, Version: rule.Version}
	}
	return registry, bindings, nil
}

func newFetchers(site, origin string, store *businessStore) (*engine.RuleSchedule, *engine.RuleFetcher, *engine.RuleFetcher, error) {
	registry, bindings, err := verifiedRules()
	if err != nil {
		return nil, nil, nil, err
	}
	cfg := engine.RuleFetcherConfig{Site: site, Stage: "detail", Registry: registry, Bindings: bindings, Origins: []string{origin},
		Transport: "http", MaxPages: 4, MaxItems: 8, ItemKey: "id", Save: store.save}
	detail, err := engine.NewRuleFetcher(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg.Stage = "catalog"
	cfg.Links = []engine.RuleLink{{Field: "href", Stage: "detail"}}
	catalog, err := engine.NewRuleFetcher(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	schedule, err := engine.NewRuleSchedule(engine.RuleScheduleConfig{Stage: "schedule", Entry: catalog, Interval: interval})
	return schedule, catalog, detail, err
}

func run(ctx context.Context, dsn string, opts options) (result demoResult, err error) {
	db, err := openDatabase(dsn)
	if err != nil {
		return result, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return result, err
	}
	defer sqlDB.Close()
	cfg := exampleConfig()
	if opts.Migrate {
		if err = database.Migrate(db.WithContext(ctx), cfg); err != nil {
			return result, fmt.Errorf("explicit example migration: %w", err)
		}
	}
	tables := database.FrameworkModels(cfg)
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		names = append(names, table.Name)
	}
	existing, err := database.ExistingTables(db.WithContext(ctx), names)
	if err != nil {
		return result, err
	}
	for _, name := range names {
		if !existing[name] {
			return result, fmt.Errorf("ruleperiodic: missing %s; explicitly run with -migrate first", name)
		}
	}
	site := newLocalSite()
	defer site.Close()
	store := &businessStore{records: make(map[string]businessRecord)}
	result.Site, result.IntervalSecond = "periodic-"+uuid.NewString(), int(interval/time.Second)
	schedule, catalog, detail, err := newFetchers(result.Site, site.URL, store)
	if err != nil {
		return result, err
	}
	logs := quietLoggers()
	e := engine.NewEngine(db, cfg, &logs)
	stopped := false
	defer func() {
		if !stopped {
			if drained, _ := e.Stop(5 * time.Second); !drained && err == nil {
				err = errors.New("example workers did not drain")
			}
		}
	}()
	e.SetSite(core.Site{Key: result.Site, BaseURL: site.URL})
	e.SetHTMLClient()
	for _, fetcher := range []*engine.RuleFetcher{catalog, detail} {
		e.AddStage(fetcher.GetStage(), engine.StageConfig{Site: result.Site, WorkerCount: 2, QueueSize: 32, MaxAttempts: 1}, fetcher, nil)
	}
	// Only the controller repeats. A tick can just wait for active members, so a
	// repeat limit would bound polls rather than the number of completed batches.
	e.AddStage("schedule", engine.StageConfig{Site: result.Site, WorkerCount: 1, QueueSize: 8, MaxAttempts: 1}, schedule, nil)
	e.ApplyRegisterStage()
	control, err := schedule.NewTask(engine.RuleRequest{URL: site.URL + "/list1"}, nil)
	if err != nil {
		return result, err
	}
	if err = e.SubmitTask(control); err != nil {
		return result, err
	}
	result.ControlID = uint(control.ID)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		status, err := waitBatch(ctx, schedule, e, result.ControlID, sequence)
		if err != nil {
			return result, err
		}
		members, err := batchMembers(ctx, db, result.Site, status.BatchID)
		if err != nil {
			return result, err
		}
		result.Batches = append(result.Batches, batchResult{Status: status, TaskIDs: members, Records: store.snapshot()})
		if sequence == 1 {
			// Change the server only after every task in the first batch succeeded.
			// The second batch is triggered by the real ten-second repeat queue.
			site.version.Store(2)
		}
	}
	if err = e.SetTaskRepeatable(result.ControlID, false); err != nil {
		return result, err
	}
	if drained, _ := e.Stop(5 * time.Second); !drained {
		return result, errors.New("example workers did not drain")
	}
	stopped = true
	result.Requests = site.requests()
	var persisted models.CrawlerTask
	if err = db.WithContext(ctx).First(&persisted, result.ControlID).Error; err != nil {
		return result, err
	}
	result.ControlRepeat = persisted.Repeat
	if err = sqlDB.Close(); err != nil {
		return result, err
	}
	// Reopen the database and rebuild the schedule/verified Registry. Status
	// reads persisted batches without starting workers or replaying requests.
	reopenedDB, err := openDatabase(dsn)
	if err != nil {
		return result, err
	}
	reopenedSQL, err := reopenedDB.DB()
	if err != nil {
		return result, err
	}
	defer reopenedSQL.Close()
	reopenedSchedule, _, _, err := newFetchers(result.Site, site.URL, store)
	if err != nil {
		return result, err
	}
	reopenedEngine := engine.NewEngine(reopenedDB, cfg, &logs)
	defer reopenedEngine.Stop(5 * time.Second)
	result.Reopened, err = reopenedSchedule.Status(ctx, reopenedEngine, result.ControlID)
	if err != nil {
		return result, err
	}
	result.ReopenedEqual = reflect.DeepEqual(result.Reopened, result.Batches[1].Status)
	if !result.ReopenedEqual || !reflect.DeepEqual(result.Requests, site.requests()) {
		return result, errors.New("reopening changed persisted status or sent another request")
	}
	return result, nil
}

func waitBatch(ctx context.Context, schedule *engine.RuleSchedule, e *engine.Engine, id uint, sequence uint64) (engine.RuleScheduleStatus, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := schedule.Status(ctx, e, id)
		if err != nil {
			return status, err
		}
		if status.Sequence > sequence || status.Failed != 0 {
			return status, fmt.Errorf("unexpected batch state: %+v", status)
		}
		if status.Sequence == sequence && status.Complete {
			if status.Total != 4 || status.Success != 4 {
				return status, fmt.Errorf("batch has %d tasks/%d successes, want 4/4", status.Total, status.Success)
			}
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, fmt.Errorf("waiting for batch %d: %w (last=%+v)", sequence, ctx.Err(), status)
		case <-ticker.C:
		}
	}
}

func batchMembers(ctx context.Context, db *gorm.DB, site, batch string) ([]uint, error) {
	var tasks []models.CrawlerTask
	if err := db.WithContext(ctx).Where("site = ? AND stage <> ?", site, "schedule").Order("id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	var ids []uint
	for _, task := range tasks {
		var meta map[string]string
		if err := json.Unmarshal(task.Meta, &meta); err != nil {
			return nil, err
		}
		if meta["_rule_batch"] == batch {
			if task.Status != models.TaskStatusSuccess || task.Repeatable != models.RepeatableNo {
				return nil, errors.New("batch member is not a successful, one-shot task")
			}
			ids = append(ids, task.ID)
		}
	}
	if len(ids) != 4 {
		return nil, fmt.Errorf("batch has %d persisted members, want 4", len(ids))
	}
	return ids, nil
}

func quietLoggers() loggers.LoggerSet {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return loggers.LoggerSet{Sys: l, Engine: l, Monitor: l, Browser: l, Fetcher: l, DB: l, Scheduler: l, Proxy: l, Filedown: l, M3U8: l}
}
