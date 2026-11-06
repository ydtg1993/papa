// rulequality runs a complete, explicitly approved feedback loop against its
// own loopback pages. The only publication is the initial host-authored v1;
// reanalysis produces a reviewed candidate and never changes that release.
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
	"os"
	"path/filepath"
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
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	q "github.com/ydtg1993/papa/v3/pkg/rulequality"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type options struct {
	DataDir, InspectRequest string
	Migrate                 bool
}

func main() {
	var opts options
	flag.StringVar(&opts.DataDir, "data-dir", "", "persistent directory for approved samples and real worker snapshots")
	flag.BoolVar(&opts.Migrate, "migrate", false, "explicitly migrate the dedicated quality_test database before running")
	flag.StringVar(&opts.InspectRequest, "inspect-request", "", "read a persisted request, report and Agent audit without executing it")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := command(ctx, os.Stdout, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, out io.Writer, opts options) error {
	dsn := os.Getenv("PAPA_QUALITY_DSN")
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || dsn == "" || !strings.Contains(parsed.DBName, "quality_test") {
		return errors.New("rulequality: PAPA_QUALITY_DSN must name a dedicated MySQL database containing quality_test")
	}
	if opts.InspectRequest == "" && opts.DataDir == "" || opts.InspectRequest != "" && opts.Migrate {
		return errors.New("rulequality: supply -data-dir for a run; inspection cannot migrate")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return errors.New("rulequality: cannot connect to the dedicated example database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if opts.Migrate {
		if err := database.Migrate(db.WithContext(ctx), exampleConfig(opts.DataDir)); err != nil {
			return fmt.Errorf("explicit example migration: %w", err)
		}
	}
	if opts.InspectRequest != "" {
		view, err := inspect(ctx, db, opts.InspectRequest)
		if err != nil {
			return err
		}
		return printJSON(out, view)
	}
	result, err := run(ctx, db, opts.DataDir)
	if err != nil {
		return err
	}
	return printJSON(out, result)
}

func exampleConfig(dir string) *config.Config {
	return &config.Config{
		Rules: config.RulesConfig{Enabled: true, SampleDir: filepath.Join(dir, "samples")},
		Agent: config.AgentConfig{Enabled: true}, Quality: config.QualityConfig{Enabled: true},
		HTML: config.HTMLConfig{Enable: true, Timeout: 5 * time.Second, MaxBodySize: 1 << 20},
		Crawler: config.CrawlerConfig{DrainInterval: 10 * time.Millisecond, DedupCacheSize: 1024,
			Archive: config.ArchiveConfig{Enabled: true, Mode: config.ArchiveModeAlways, Dir: filepath.Join(dir, "archive"), Retention: -1}},
	}
}

type scenarioResult struct {
	Name            string   `json:"name"`
	Report          q.Report `json:"report"`
	SuccessfulTasks int64    `json:"successful_tasks"`
	FailedTasks     int64    `json:"failed_tasks"`
	NormalStops     int      `json:"normal_stops"`
}

type demoResult struct {
	Site                          string                    `json:"site"`
	Mode                          string                    `json:"mode"`
	Scenarios                     []scenarioResult          `json:"scenarios"`
	Request                       q.Request                 `json:"request"`
	Run                           crawlagent.Result         `json:"run"`
	Candidate                     rulestore.Reference       `json:"candidate"`
	Verified                      bool                      `json:"verified"`
	InitialRelease                crawlrule.ReleaseSnapshot `json:"initial_release"`
	CurrentRelease                crawlrule.ReleaseSnapshot `json:"current_release"`
	ReleaseUnchanged              bool                      `json:"release_unchanged"`
	ReplayCalls                   int                       `json:"replay_calls"`
	ReopenedRequest               bool                      `json:"reopened_request"`
	ReplayRejected                bool                      `json:"replay_rejected"`
	MissingEvidenceStatus         string                    `json:"missing_evidence_status"`
	MissingEvidenceModelCalls     int                       `json:"missing_evidence_model_calls"`
	QualityFailures               uint64                    `json:"quality_failures"`
	SaveCallsDuringQualityFailure int64                     `json:"save_calls_during_quality_failure"`
	UnrelatedReportsRejected      bool                      `json:"unrelated_reports_rejected"`
}

type faultRecorder struct {
	q.Recorder
	fail atomic.Bool
}

func (r *faultRecorder) Record(ctx context.Context, o q.Observation) (q.Observation, error) {
	if r.fail.Load() {
		return q.Observation{}, errors.New("example injected quality storage failure")
	}
	return r.Recorder.Record(ctx, o)
}

func run(ctx context.Context, db *gorm.DB, dir string) (result demoResult, err error) {
	srv := newLocalSite()
	defer srv.Close()
	cfg := exampleConfig(dir)
	blobs, err := rulestore.NewFileBlobs(cfg.Rules.SampleDir)
	if err != nil {
		return result, err
	}
	defer blobs.Close()
	repo, err := rulestore.New(db, blobs)
	if err != nil {
		return result, err
	}
	store, err := q.New(db)
	if err != nil {
		return result, err
	}
	audit, err := agentstore.New(db)
	if err != nil {
		return result, err
	}
	actor := rulestore.Actor{Operator: "example-host", Source: "rulequality authored loopback fixtures"}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) {
		// Approval is limited to the public fixture pages authored below. Real
		// sites need their own disclosure/redaction policy.
		return append([]byte(nil), body...), nil
	}}
	result.Site, result.Mode = "quality-"+uuid.NewString(), "deterministic-replay"
	ruleID := result.Site + "-list"
	v1 := exampleRule(ruleID, "v1", ".title")
	sets, err := prepareFixtures(ctx, repo, actor, policy, srv.URL, result.Site)
	if err != nil {
		return result, err
	}
	if _, err := repo.CreateDraft(ctx, v1, actor); err != nil {
		return result, err
	}
	ref := rulestore.Reference{ID: ruleID, Version: "v1"}
	if _, err := repo.Verify(ctx, ref, sets.baseline, actor); err != nil {
		return result, err
	}
	if _, err := repo.Publish(ctx, result.Site+"-publish", result.Site, 0, map[string]crawlrule.Reference{"list": ref}, actor); err != nil {
		return result, err
	}
	result.InitialRelease, err = repo.Snapshot(ctx, result.Site)
	if err != nil {
		return result, err
	}
	fault := &faultRecorder{Recorder: store}
	collector, err := q.NewCollector(fault, 0)
	if err != nil {
		return result, err
	}
	var saved sync.Map // idempotent demo upsert; no business schema is created
	var saves atomic.Int64
	fetcher, err := engine.NewRuleFetcher(engine.RuleFetcherConfig{Site: result.Site, Stage: "list", Catalog: repo,
		Origins: []string{srv.URL}, ItemKey: "id", SnapshotPolicy: &policy, Quality: collector,
		Next: func(_ context.Context, _ *engine.Task, page engine.RulePage) (*engine.RuleRequest, error) {
			if strings.Contains(page.SourceURL, "/duplicates/") && !strings.HasSuffix(page.SourceURL, "/second") {
				return &engine.RuleRequest{URL: page.SourceURL + "/second"}, nil
			}
			return nil, nil
		},
		Save: func(_ context.Context, _ *engine.Task, page engine.RulePage) error {
			saves.Add(1)
			if strings.Contains(page.SourceURL, "/save/") {
				return errors.New("example injected business save failure")
			}
			for _, row := range page.Result.Records {
				saved.Store(row["id"], row)
			}
			return nil
		}})
	if err != nil {
		return result, err
	}
	logs := quietLoggers()
	e := engine.NewEngine(db, cfg, &logs)
	defer func() {
		if drained, _ := e.Stop(5 * time.Second); !drained && err == nil {
			err = errors.New("example worker shutdown did not drain")
		}
	}()
	e.SetSite(core.Site{Key: result.Site, BaseURL: srv.URL})
	e.SetHTMLClient()
	e.AddStage("list", engine.StageConfig{Site: result.Site, WorkerCount: 2, QueueSize: 128, MaxAttempts: 1}, fetcher, nil)
	e.ApplyRegisterStage()
	scope := q.Scope{Site: result.Site, Stage: "list", RuleID: ruleID, Version: "v1", ReleaseID: result.InitialRelease.ID, ReleaseGeneration: result.InitialRelease.Generation}
	qualityPolicy := q.DefaultPolicy()
	missing := .25
	qualityPolicy.MissingRate = &missing // host expectation for optional title coverage
	var baselineID string
	var drift q.Report
	totalTasks := int64(0)
	for _, name := range []string{"normal", "missing", "rate", "duplicates", "save"} {
		from, err := databaseNow(ctx, db)
		if err != nil {
			return result, err
		}
		for i := 0; i < 20; i++ {
			task, err := fetcher.NewTaskContext(ctx, engine.RuleRequest{URL: fmt.Sprintf("%s/%s/%d", srv.URL, name, i)}, nil)
			if err != nil {
				return result, err
			}
			if err = e.SubmitTask(task); err != nil {
				return result, err
			}
		}
		expected := int64(20)
		if name == "duplicates" {
			expected = 40
		}
		totalTasks += expected
		if err := waitTasks(ctx, db, result.Site, totalTasks); err != nil {
			return result, err
		}
		until, err := databaseNow(ctx, db)
		if err != nil {
			return result, err
		}
		compare := ""
		if name == "missing" {
			compare = baselineID
		}
		query := q.Query{Scope: scope, From: from, Until: until, LostObservations: collector.Stats().Failed}
		report, err := q.BuildReport(ctx, store, query, qualityPolicy, compare)
		if err != nil {
			return result, fmt.Errorf("%s report: %w", name, err)
		}
		window, err := store.Query(ctx, query)
		if err != nil {
			return result, err
		}
		item := scenarioResult{Name: name, Report: report}
		ids := make([]int, 0, len(window.Observations))
		for _, observation := range window.Observations {
			ids = append(ids, observation.TaskID)
			if observation.StopReason == "repeated-content" {
				item.NormalStops++
			}
		}
		if err := db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("site = ? AND id IN ? AND status = ?", result.Site, ids, models.TaskStatusSuccess).Count(&item.SuccessfulTasks).Error; err != nil {
			return result, err
		}
		if err := db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("site = ? AND id IN ? AND status = ?", result.Site, ids, models.TaskStatusFailed).Count(&item.FailedTasks).Error; err != nil {
			return result, err
		}
		want := map[string]string{"normal": q.ObservedOK, "missing": q.SuspectedDrift, "rate": q.UpstreamFailure, "duplicates": q.ObservedOK, "save": q.ProcessingFailure}[name]
		if report.Verdict != want {
			return result, fmt.Errorf("%s verdict=%s, want %s", name, report.Verdict, want)
		}
		if name == "normal" {
			baselineID = report.ID
		}
		if name == "missing" {
			drift = report
		}
		result.Scenarios = append(result.Scenarios, item)
	}
	// A recorder outage is injected after the fixed reports. Business saving
	// still occurs once; its missing observation is exposed by Collector stats.
	fault.fail.Store(true)
	before := saves.Load()
	task, err := fetcher.NewTaskContext(ctx, engine.RuleRequest{URL: srv.URL + "/quality-unavailable/one"}, nil)
	if err != nil {
		return result, err
	}
	if err := e.SubmitTask(task); err != nil {
		return result, err
	}
	if err := waitTasks(ctx, db, result.Site, totalTasks+1); err != nil {
		return result, err
	}
	fault.fail.Store(false)
	result.SaveCallsDuringQualityFailure, result.QualityFailures = saves.Load()-before, collector.Stats().Failed
	if result.SaveCallsDuringQualityFailure != 1 || result.QualityFailures != 1 {
		return result, errors.New("quality failure changed business execution")
	}
	provider := model.NewReplay(replaySteps()...)
	agent, err := crawlagent.New(crawlagent.Config{Model: provider, ModelName: "deterministic-replay", Audit: audit, Repository: repo, Blobs: blobs, Actor: actor, SamplePolicy: policy,
		Web:     webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: srv.URL, AllowPrivateNetwork: true}}}},
		Budgets: crawlagent.Budgets{MaxModelCalls: 4, MaxDuration: 30 * time.Second}})
	if err != nil {
		return result, err
	}
	bridgeConfig := q.ReanalysisConfig{Store: store, Agent: agent, Audit: audit, Rules: repo, ArchiveDir: cfg.Crawler.Archive.Dir, SamplePolicy: policy, Actor: actor}
	bridge, err := q.NewReanalyzer(bridgeConfig)
	if err != nil {
		return result, err
	}
	result.UnrelatedReportsRejected = true
	for _, scenario := range result.Scenarios {
		if scenario.Name == "missing" {
			continue
		}
		_, err := bridge.Prepare(ctx, scenario.Report.ID, result.Site+"-reject-"+scenario.Name)
		result.UnrelatedReportsRejected = result.UnrelatedReportsRejected && errors.Is(err, q.ErrNotActionable)
	}
	request, err := bridge.Prepare(ctx, drift.ID, result.Site+"-explicit-review")
	if err != nil {
		return result, err
	}
	duplicate, err := bridge.Prepare(ctx, drift.ID, result.Site+"-explicit-review")
	if err != nil || duplicate.ID != request.ID || duplicate.RunID != request.RunID {
		return result, errors.New("explicit operation was not idempotent")
	}
	goal := crawlagent.Goal{Site: result.Site, Seeds: []string{srv.URL + "/missing/0"}, Stages: []crawlagent.StageGoal{{Stage: "list", RuleID: ruleID, Transport: "http", ItemKey: "id", Existing: &ref,
		Fields: []crawlagent.FieldGoal{{Name: "id", Kind: "string", Required: true}, {Name: "title", Kind: "string"}}, TrainingSet: sets.training, HoldoutSet: sets.holdout}}}
	if len(drift.Evidence) == 0 || drift.Evidence[0].EvidenceID == "" {
		return result, errors.New("real worker evidence was not retained")
	}
	input := q.ReanalysisInput{Goal: goal, EvidenceIDs: []string{drift.Evidence[0].EvidenceID}}
	outcome, err := bridge.Execute(ctx, request.ID, request.Revision, input)
	if err != nil {
		return result, fmt.Errorf("explicit replay reanalysis: %w", err)
	}
	result.Request, result.Run, result.Verified = outcome.Request, outcome.Analysis, outcome.Analysis.Verified
	if len(outcome.Analysis.Candidates) != 1 {
		return result, errors.New("expected one independently checked candidate")
	}
	result.Candidate = outcome.Analysis.Candidates[0].Reference
	version, err := repo.Get(ctx, result.Candidate)
	if err != nil || version.EverPublished || !result.Verified || result.Request.Status != q.AwaitingReview {
		return result, errors.New("candidate was unverified or unexpectedly published")
	}
	// Reopen durable adapters, then prove that reading or resubmitting a finished
	// request does not authorize a second model run.
	reopened, err := q.New(db.Session(&gorm.Session{NewDB: true}))
	if err != nil {
		return result, err
	}
	stored, err := reopened.Request(ctx, request.ID)
	if err != nil {
		return result, err
	}
	result.ReopenedRequest = stored.ID == result.Request.ID && stored.RunID == result.Run.RunID && stored.Status == q.AwaitingReview
	bridgeConfig.Store = reopened
	bridge, err = q.NewReanalyzer(bridgeConfig)
	if err != nil {
		return result, err
	}
	callsBefore := len(provider.Calls())
	_, err = bridge.Execute(ctx, request.ID, request.Revision, input)
	result.ReplayRejected = errors.Is(err, q.ErrConflict) && len(provider.Calls()) == callsBefore
	// An explicitly missing archive is a separate requested investigation. It
	// never reconstructs snapshots or fetches production pages to fill the gap.
	missingRequest, err := bridge.Prepare(ctx, drift.ID, result.Site+"-missing-evidence")
	if err != nil {
		return result, err
	}
	bridgeConfig.ArchiveDir = filepath.Join(dir, "deliberately-absent-archive")
	missingBridge, err := q.NewReanalyzer(bridgeConfig)
	if err != nil {
		return result, err
	}
	missingOutcome, missingErr := missingBridge.Execute(ctx, missingRequest.ID, missingRequest.Revision, input)
	result.MissingEvidenceStatus = missingOutcome.Request.Status
	result.MissingEvidenceModelCalls = len(provider.Calls()) - callsBefore
	if !errors.Is(missingErr, q.ErrEvidence) || result.MissingEvidenceStatus != q.NeedsEvidence || result.MissingEvidenceModelCalls != 0 {
		return result, errors.New("missing evidence unexpectedly invoked the Agent")
	}
	result.CurrentRelease, err = repo.Snapshot(ctx, result.Site)
	if err != nil {
		return result, err
	}
	result.ReleaseUnchanged = result.InitialRelease.ID == result.CurrentRelease.ID && result.InitialRelease.Generation == result.CurrentRelease.Generation && result.CurrentRelease.Bindings["list"] == ref
	result.ReplayCalls = len(provider.Calls())
	if !result.ReleaseUnchanged || !result.ReplayRejected || !result.ReopenedRequest || !result.UnrelatedReportsRejected || result.ReplayCalls != 4 {
		return result, errors.New("feedback workflow invariants failed")
	}
	return result, nil
}

type fixtureSets struct{ baseline, training, holdout string }

func prepareFixtures(ctx context.Context, repo *rulestore.Repository, actor rulestore.Actor, policy htmlfetch.SamplePolicy, base, prefix string) (fixtureSets, error) {
	sets := fixtureSets{prefix + "-baseline", prefix + "-training", prefix + "-holdout"}
	for _, fixture := range []struct{ set, path, body, id, title string }{
		{sets.baseline, "/fixtures/baseline", `<article data-id="old"><span class="title">Old title</span></article>`, "old", "Old title"},
		{sets.training, "/fixtures/training", `<article data-id="train"><h2 class="heading">Training title</h2></article>`, "train", "Training title"},
		{sets.holdout, "/fixtures/holdout", `<article data-id="hidden"><header class="heading">Independent title</header></article>`, "hidden", "Independent title"},
	} {
		sample, err := repo.AddSample(ctx, rulestore.SampleInput{URL: base + fixture.path, StatusCode: 200, Body: []byte(fixture.body)}, policy, actor)
		if err != nil {
			return sets, err
		}
		want := crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/id", Value: fixture.id}, {Pointer: "/title", Value: fixture.title}}}
		if err := repo.CreateSampleSet(ctx, fixture.set, []rulestore.StoredFixture{{Name: "host-authored", SampleID: sample.ID, Expect: want}}, actor); err != nil {
			return sets, err
		}
	}
	return sets, nil
}

func exampleRule(id, version, titleSelector string) crawlrule.Rule {
	return crawlrule.Rule{SchemaVersion: 1, ID: id, Version: version, Format: "html", Rows: "article", Fields: []crawlrule.Field{
		{Name: "id", Attribute: "data-id", Validation: crawlrule.Validation{Kind: "string", Required: true}},
		{Name: "title", Selector: titleSelector, Validation: crawlrule.Validation{Kind: "string"}},
	}, Next: &crawlrule.Next{Selector: "a.next", Attribute: "href"}}
}

func replaySteps() []model.ReplayStep {
	// Rule identity/version belong to the host. A concrete candidate object
	// omits them instead of serializing empty prohibited fields.
	rule := map[string]any{"schema_version": 1, "format": "html", "rows": "article", "fields": exampleRule("", "", ".title,.heading").Fields,
		"next": map[string]any{"selector": "a.next", "attribute": "href"}}
	actions := []map[string]any{{"tool": "read_evidence", "evidence_id": "latest"}, {"tool": "submit_candidate", "stage": "list", "rule": rule}, {"tool": "validate_candidate", "stage": "list"}, {"tool": "finish", "note": "Host-authored replay: preserve candidate for human review."}}
	steps := make([]model.ReplayStep, 0, len(actions))
	for _, action := range actions {
		raw, _ := json.Marshal(action)
		steps = append(steps, model.ReplayStep{Reply: model.Reply{Content: string(raw)}})
	}
	return steps
}

func newLocalSite() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		if parts[0] == "rate" {
			w.WriteHeader(429)
			fmt.Fprint(w, "Local rate limit fixture")
			return
		}
		selector := "title"
		if parts[0] == "missing" {
			selector = "heading"
		}
		fmt.Fprintf(w, `<article data-id="%s-%s"><span class="%s">Local title</span></article>`, parts[0], parts[1], selector)
	}))
}

func databaseNow(ctx context.Context, db *gorm.DB) (time.Time, error) {
	var now time.Time
	err := db.WithContext(ctx).Raw("SELECT CURRENT_TIMESTAMP(6)").Row().Scan(&now)
	return now.UTC(), err
}

func waitTasks(ctx context.Context, db *gorm.DB, site string, want int64) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var completed int64
		if err := db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("site = ? AND status IN ?", site, []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}).Count(&completed).Error; err != nil {
			return err
		}
		if completed == want {
			return nil
		}
		if completed > want {
			return errors.New("example task count exceeded its bounded plan")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type inspection struct {
	Request q.Request      `json:"request"`
	Report  q.Report       `json:"report"`
	Run     agentstore.Run `json:"run"`
}

func inspect(ctx context.Context, db *gorm.DB, id string) (out inspection, err error) {
	store, err := q.New(db)
	if err != nil {
		return out, err
	}
	if out.Request, err = store.Request(ctx, id); err != nil {
		return out, err
	}
	if out.Report, err = store.Report(ctx, out.Request.ReportID); err != nil {
		return out, err
	}
	audit, err := agentstore.New(db)
	if err != nil {
		return out, err
	}
	out.Run, err = audit.Get(ctx, out.Request.RunID)
	if errors.Is(err, agentstore.ErrNotFound) {
		err = nil
	}
	return out, err
}

func quietLoggers() loggers.LoggerSet {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return loggers.LoggerSet{Sys: l, Engine: l, Monitor: l, Browser: l, Fetcher: l, DB: l, Scheduler: l, Proxy: l, Filedown: l, M3U8: l}
}

func printJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
