// siteagent runs a bounded analysis against its own public loopback fixtures.
// It persists candidates and audit records but never publishes a release.
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
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type options struct {
	SampleDir, BrowserPath, BaseURL, Model, APIKeyEnv string
	InspectRun, InterruptRun                          string
	Migrate, Render, RealModel, LegacyMaxTokens       bool
	WorkerStopped                                     bool
	Evaluate                                          bool
	EvalCompare, PriceVersion, Currency               string
	InputPrice, OutputPrice, MaxCost                  int64
}

func main() {
	var opts options
	flag.StringVar(&opts.SampleDir, "sample-dir", "", "persistent directory for approved public evidence")
	flag.BoolVar(&opts.Migrate, "migrate", false, "explicitly migrate the dedicated example database")
	flag.BoolVar(&opts.Render, "render", true, "include controlled DOM rendering and observed GET JSON")
	flag.StringVar(&opts.BrowserPath, "browser", "", "optional installed Chromium executable")
	flag.BoolVar(&opts.RealModel, "real-model", false, "explicitly enable billable compatible model calls; default is replay")
	flag.StringVar(&opts.BaseURL, "model-base-url", os.Getenv("PAPA_AGENT_MODEL_BASE_URL"), "compatible HTTPS API base URL, including its configured version path")
	flag.StringVar(&opts.Model, "model", os.Getenv("PAPA_AGENT_MODEL"), "host-selected model name")
	flag.StringVar(&opts.APIKeyEnv, "api-key-env", "PAPA_AGENT_MODEL_KEY", "environment variable containing the model key")
	flag.BoolVar(&opts.LegacyMaxTokens, "legacy-max-tokens", false, "send legacy max_tokens instead of max_completion_tokens")
	flag.StringVar(&opts.InspectRun, "inspect-run", "", "read a persisted run and its steps without executing it")
	flag.StringVar(&opts.InterruptRun, "interrupt-run", "", "explicitly mark a stopped run interrupted; never resume it")
	flag.BoolVar(&opts.WorkerStopped, "worker-stopped", false, "confirm the previous executor has stopped before interruption")
	flag.BoolVar(&opts.Evaluate, "evaluate", false, "run fixed HTML/DOM/JSON evaluation: 3 repetitions each, no publication")
	flag.StringVar(&opts.EvalCompare, "eval-compare", "", "compare with a complete evaluation JSON report of identical identity")
	flag.StringVar(&opts.PriceVersion, "price-version", "", "host price schedule version; required for real-model evaluation")
	flag.StringVar(&opts.Currency, "currency", "", "price currency label; required for real-model evaluation")
	flag.Int64Var(&opts.InputPrice, "input-price-micros", 0, "micro currency units per million input tokens")
	flag.Int64Var(&opts.OutputPrice, "output-price-micros", 0, "micro currency units per million output tokens")
	flag.Int64Var(&opts.MaxCost, "max-run-micros", 0, "estimated cost ceiling per analysis; whole evaluation reserves at most 9 times this")
	flag.Parse()
	deadline := 210 * time.Second
	if opts.Evaluate {
		deadline = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if err := command(ctx, os.Stdout, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, output io.Writer, opts options) error {
	if err := validateEvaluationOptions(opts); err != nil {
		return err
	}
	if opts.InspectRun != "" && opts.InterruptRun != "" {
		return errors.New("choose only one of -inspect-run and -interrupt-run")
	}
	if opts.InterruptRun != "" && !opts.WorkerStopped {
		return errors.New("-interrupt-run requires -worker-stopped after confirming the old executor has ended")
	}
	dsn := os.Getenv("PAPA_AGENT_DSN")
	if dsn == "" || (opts.SampleDir == "" && (opts.Migrate || opts.InspectRun == "" && opts.InterruptRun == "")) {
		return errors.New("set PAPA_AGENT_DSN to a dedicated MySQL database and supply -sample-dir for analysis or migration")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return errors.New("siteagent: cannot connect to the dedicated MySQL database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if opts.Migrate {
		cfg := &config.Config{Rules: config.RulesConfig{Enabled: true, SampleDir: opts.SampleDir}, Agent: config.AgentConfig{Enabled: true}}
		if err := database.Migrate(db.WithContext(ctx), cfg); err != nil {
			return fmt.Errorf("explicit example migration: %w", err)
		}
	}
	if opts.InspectRun != "" || opts.InterruptRun != "" {
		store, err := agentstore.New(db)
		if err != nil {
			return err
		}
		id := opts.InspectRun
		if opts.InterruptRun != "" {
			id = opts.InterruptRun
			run, err := store.Get(ctx, id)
			if err != nil {
				return err
			}
			if _, err = store.Interrupt(ctx, id, run.Revision); err != nil {
				return err
			}
		}
		view, err := inspect(ctx, store, id)
		if err != nil {
			return err
		}
		return printJSON(output, view)
	}
	if opts.Evaluate {
		return runEvaluation(ctx, output, db, opts)
	}
	return run(ctx, output, db, opts)
}

type auditView struct {
	Run   agentstore.Run    `json:"run"`
	Steps []agentstore.Step `json:"steps"`
}

func inspect(ctx context.Context, store agentstore.Store, id string) (auditView, error) {
	run, err := store.Get(ctx, id)
	if err != nil {
		return auditView{}, err
	}
	steps, err := store.Steps(ctx, id, 256)
	return auditView{Run: run, Steps: steps}, err
}

type demoResult struct {
	crawlagent.Result
	Site                  string `json:"site"`
	Mode                  string `json:"mode"`
	Rendering             bool   `json:"rendering"`
	Unpublished           bool   `json:"unpublished"`
	RestoredAuditSteps    int    `json:"restored_audit_steps"`
	RestoredCandidates    int    `json:"restored_candidates"`
	InterruptionExampleID string `json:"interruption_example_id"`
	PendingBecameUnknown  bool   `json:"pending_became_unknown"`
}

func run(ctx context.Context, output io.Writer, db *gorm.DB, opts options) error {
	site := newLocalSite()
	defer site.Close()
	blobs, err := rulestore.NewFileBlobs(opts.SampleDir)
	if err != nil {
		return err
	}
	defer blobs.Close()
	repo, err := rulestore.New(db, blobs)
	if err != nil {
		return err
	}
	audit, err := agentstore.New(db)
	if err != nil {
		return err
	}
	prefix := "agent-demo-" + uuid.NewString()
	actor := rulestore.Actor{Operator: "example-host", Source: "siteagent public loopback fixtures"}
	policy := htmlfetch.SamplePolicy{QueryKeys: []string{"page"}, RedactBody: func(_ bool, body []byte) ([]byte, error) {
		// This explicit approval applies only to this example's authored public
		// fixture server. A real site's body requires a suitable redactor.
		return append([]byte(nil), body...), nil
	}}
	stages := stageFixtures(site.URL, opts.Render)
	goal := crawlagent.Goal{Site: prefix}
	for _, fixture := range stages {
		stage := fixture.goal
		stage.RuleID = prefix + "-" + stage.Stage
		stage.TrainingSet, stage.HoldoutSet = prefix+"-"+stage.Stage+"-train", prefix+"-"+stage.Stage+"-holdout"
		for _, set := range []struct {
			id    string
			input rulestore.SampleInput
			want  crawlrule.Expectations
		}{{stage.TrainingSet, fixture.training, fixture.trainingWant}, {stage.HoldoutSet, fixture.holdout, fixture.holdoutWant}} {
			sample, err := repo.AddSample(ctx, set.input, policy, actor)
			if err != nil {
				return err
			}
			emptyInput := set.input
			emptyInput.URL += "/empty"
			emptyInput.Body = []byte("<main>Empty fixture " + set.id + "</main>")
			if fixture.rule.Format == "json" {
				emptyInput.Body, _ = json.Marshal(map[string]any{"items": []any{}, "marker": set.id})
			}
			empty, err := repo.AddSample(ctx, emptyInput, policy, actor)
			if err != nil {
				return err
			}
			fixtures := []rulestore.StoredFixture{
				{Name: stage.Stage, SampleID: sample.ID, Expect: set.want},
				{Name: stage.Stage + "-empty", SampleID: empty.ID, Expect: crawlrule.Expectations{Kind: "empty"}},
			}
			if err := repo.CreateSampleSet(ctx, set.id, fixtures, actor); err != nil {
				return err
			}
		}
		goal.Stages = append(goal.Stages, stage)
		goal.Seeds = append(goal.Seeds, site.URL+fixture.seed)
	}
	provider, modelName, err := selectProvider(opts, replaySteps(site.URL, stages, opts.Render))
	if err != nil {
		return err
	}
	origin := webaccess.Origin{URL: site.URL, AllowPrivateNetwork: true, PathPrefixes: []string{"/list", "/detail", "/render", "/json", "/api", "/favicon.ico"}}
	agent, err := crawlagent.New(crawlagent.Config{
		Model: provider, ModelName: modelName, Audit: audit, Repository: repo, Blobs: blobs, Actor: actor, SamplePolicy: policy,
		AllowRecordAPI: opts.Render,
		Web: webaccess.Config{
			Policy:   webaccess.Policy{Origins: []webaccess.Origin{origin}, ResourceOrigins: []webaccess.Origin{origin}},
			Renderer: webaccess.RendererConfig{Enabled: opts.Render, ExecutablePath: opts.BrowserPath, GlobalConcurrency: 1},
		},
		Budgets: crawlagent.Budgets{MaxModelCalls: 20, MaxDuration: 180 * time.Second},
	})
	if err != nil {
		return err
	}
	analysis, runErr := agent.Run(ctx, goal)
	mode := "replay"
	if opts.RealModel {
		mode = "real-compatible-model"
	}
	result := demoResult{Result: analysis, Site: prefix, Mode: mode, Rendering: opts.Render}
	stepsSucceeded := true
	// Reopen adapters and body storage. No provider call or automatic resume is
	// part of this inspection, even if the persisted run did not finish.
	reopenedBlobs, err := rulestore.NewFileBlobs(opts.SampleDir)
	if err != nil {
		return err
	}
	defer reopenedBlobs.Close()
	reopenedRepo, err := rulestore.New(db.Session(&gorm.Session{NewDB: true}), reopenedBlobs)
	if err != nil {
		return err
	}
	reopenedAudit, err := agentstore.New(db.Session(&gorm.Session{NewDB: true}))
	if err != nil {
		return err
	}
	if analysis.RunID != "" {
		view, err := inspect(ctx, reopenedAudit, analysis.RunID)
		if err != nil {
			return err
		}
		result.RestoredAuditSteps = len(view.Steps)
		for _, step := range view.Steps {
			stepsSucceeded = stepsSucceeded && step.Status == agentstore.StepSucceeded
		}
		if view.Run.Status != analysis.Status {
			return errors.New("siteagent: restored run status differs from analysis result")
		}
	}
	for _, candidate := range analysis.Candidates {
		version, err := reopenedRepo.Get(ctx, candidate.Reference)
		if err != nil || version.EverPublished {
			return errors.New("siteagent: candidate missing or unexpectedly published")
		}
		result.RestoredCandidates++
	}
	_, currentErr := reopenedRepo.Current(ctx, prefix)
	result.Unpublished = errors.Is(currentErr, rulestore.ErrNoRelease)
	if !result.Unpublished {
		return errors.New("siteagent: example must not create a production release")
	}
	if runErr == nil {
		result.InterruptionExampleID, result.PendingBecameUnknown, err = demonstrateInterruption(ctx, reopenedAudit, prefix)
		if err != nil {
			return err
		}
	}
	if err := printJSON(output, result); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("siteagent analysis ended; inspect the emitted run ID: %w", runErr)
	}
	if !opts.RealModel && (!analysis.Verified || analysis.Status != agentstore.AwaitingReview || len(analysis.Candidates) != len(stages) || !stepsSucceeded || !completeLiveEvidence(analysis.Evidence, site.URL, opts.Render)) {
		return errors.New("siteagent: replay did not satisfy its independent fixture checks")
	}
	return nil
}

func completeLiveEvidence(evidence []crawlagent.Evidence, base string, render bool) bool {
	wanted := map[string]bool{base + "/list": false, base + "/detail/alpha": false, base + "/api/items?page=1": false, base + "/api/items?page=2": false}
	if render {
		wanted[base+"/render"], wanted[base+"/json"] = false, false
	}
	for _, item := range evidence {
		if _, exists := wanted[item.URL]; exists {
			if item.URL == base+"/render" || item.URL == base+"/json" {
				wanted[item.URL] = item.Rendered && item.StatusCode == 0
			} else {
				wanted[item.URL] = !item.Rendered && item.StatusCode == 200
			}
		}
	}
	for _, found := range wanted {
		if !found {
			return false
		}
	}
	return true
}

func selectProvider(opts options, steps []model.ReplayStep) (model.Provider, string, error) {
	if !opts.RealModel {
		return model.NewReplay(steps...), "deterministic-replay", nil
	}
	field := model.MaxCompletionTokens
	if opts.LegacyMaxTokens {
		field = model.MaxTokens
	}
	provider, err := model.New(model.Config{BaseURL: opts.BaseURL, Model: opts.Model, APIKeyEnv: opts.APIKeyEnv, OutputTokenField: field, MaxInputBytes: model.HardMaxInputBytes, Timeout: 45 * time.Second})
	return provider, opts.Model, err
}

func demonstrateInterruption(ctx context.Context, store agentstore.Store, prefix string) (string, bool, error) {
	// The demo itself is the only executor and makes no external request for
	// this synthetic pending step. Its worker is therefore known to have ended.
	run, err := store.Start(ctx, agentstore.Run{ID: prefix + "-stopped", SnapshotJSON: `{"example":"simulated stopped worker; no external request"}`})
	if err != nil {
		return "", false, err
	}
	run, _, err = store.BeginStep(ctx, run.ID, run.Revision, agentstore.Step{Kind: "model", InputJSON: `{"example":"reservation only"}`}, `{}`)
	if err != nil {
		return "", false, err
	}
	if _, err := store.Interrupt(ctx, run.ID, run.Revision); err != nil {
		return "", false, err
	}
	view, err := inspect(ctx, store, run.ID)
	if err != nil {
		return "", false, err
	}
	return run.ID, view.Run.Status == agentstore.Interrupted && len(view.Steps) == 1 && view.Steps[0].Status == agentstore.StepUnknown, nil
}

func printJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type stageFixture struct {
	goal                      crawlagent.StageGoal
	seed                      string
	rule                      crawlrule.Rule
	training, holdout         rulestore.SampleInput
	trainingWant, holdoutWant crawlrule.Expectations
}

func stageFixtures(base string, render bool) []stageFixture {
	text := crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}
	urlValidation := crawlrule.Validation{Kind: "http-url", Required: true}
	absolute := []crawlrule.Transform{{Op: "absolute_url"}}
	goalField := func(name, kind string) crawlagent.FieldGoal {
		return crawlagent.FieldGoal{Name: name, Kind: kind, Required: true, Description: "Public " + name + " from the page"}
	}
	fixtures := []stageFixture{
		{goal: crawlagent.StageGoal{Stage: "list", Transport: "http", Fields: []crawlagent.FieldGoal{goalField("title", "string"), goalField("url", "http-url")}, ItemKey: "url", Links: []crawlagent.LinkGoal{{Field: "url", Stage: "detail"}}}, seed: "/list",
			rule:         crawlrule.Rule{SchemaVersion: 1, Format: "html", Rows: "article.item", Fields: []crawlrule.Field{{Name: "title", Selector: "a", Validation: text}, {Name: "url", Selector: "a", Attribute: "href", Validation: urlValidation, Transforms: absolute}}},
			training:     sample(base+"/fixtures/list/train", `<article class="item"><a href="/detail/alpha">Alpha</a></article>`, false),
			holdout:      sample(base+"/fixtures/list/holdout", `<article class="item"><a href="/detail/beta">Beta</a></article>`, false),
			trainingWant: expected("Alpha", "url", base+"/detail/alpha"), holdoutWant: expected("Beta", "url", base+"/detail/beta")},
		{goal: crawlagent.StageGoal{Stage: "detail", Transport: "http", Fields: []crawlagent.FieldGoal{goalField("title", "string"), goalField("summary", "string")}}, seed: "/detail/alpha",
			rule:         crawlrule.Rule{SchemaVersion: 1, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "h1", Validation: text}, {Name: "summary", Selector: ".summary", Validation: text}}},
			training:     sample(base+"/fixtures/detail/train", `<article><h1>Alpha detail</h1><p class="summary">A public summary.</p></article>`, false),
			holdout:      sample(base+"/fixtures/detail/holdout", `<article><h1>Beta detail</h1><p class="summary">A different summary.</p></article>`, false),
			trainingWant: expected("Alpha detail", "summary", "A public summary."), holdoutWant: expected("Beta detail", "summary", "A different summary.")},
	}
	if render {
		fixtures = append(fixtures, stageFixture{goal: crawlagent.StageGoal{Stage: "rendered", Transport: "browser", Fields: []crawlagent.FieldGoal{goalField("title", "string")}}, seed: "/render",
			rule:         crawlrule.Rule{SchemaVersion: 1, Format: "html", Rows: "section.card", Fields: []crawlrule.Field{{Name: "title", Selector: "h2", Validation: text}}},
			training:     sample(base+"/fixtures/render/train", `<html><body><section class="card"><h2>Rendered Alpha</h2></section></body></html>`, true),
			holdout:      sample(base+"/fixtures/render/holdout", `<html><body><section class="card"><h2>Rendered Beta</h2></section></body></html>`, true),
			trainingWant: expected("Rendered Alpha", "", ""), holdoutWant: expected("Rendered Beta", "", "")})
	}
	api := stageFixture{goal: crawlagent.StageGoal{Stage: "json", Transport: "http", Fields: []crawlagent.FieldGoal{goalField("title", "string"), goalField("url", "http-url")}, ItemKey: "url"}, seed: "/api/items?page=1",
		rule:     crawlrule.Rule{SchemaVersion: 1, Format: "json", Rows: "/items", Fields: []crawlrule.Field{{Name: "title", Pointer: "/title", Validation: text}, {Name: "url", Pointer: "/url", Validation: urlValidation, Transforms: absolute}}, Next: &crawlrule.Next{Pointer: "/next"}},
		training: sample(base+"/fixtures/json/train", jsonPageOne, false), holdout: sample(base+"/fixtures/json/holdout", `{"items":[{"title":"JSON Gamma","url":"/detail/gamma"}],"next":null}`, false),
		trainingWant: expected("JSON Alpha", "url", base+"/detail/alpha"), holdoutWant: expected("JSON Gamma", "url", base+"/detail/gamma")}
	api.trainingWant.NextURL = base + "/api/items?page=2"
	return append(fixtures, api)
}

func sample(url, body string, rendered bool) rulestore.SampleInput {
	status := http.StatusOK
	if rendered {
		status = 0
	}
	return rulestore.SampleInput{URL: url, Body: []byte(body), StatusCode: status, Rendered: rendered}
}

func expected(title, other, value string) crawlrule.Expectations {
	want := crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: title}}}
	if other != "" {
		want.Fields = append(want.Fields, crawlrule.FieldExpectation{Pointer: "/" + other, Value: value})
	}
	return want
}

const jsonPageOne = `{"items":[{"title":"JSON Alpha","url":"/detail/alpha"}],"next":"/api/items?page=2"}`
const jsonPageTwo = `{"items":[{"title":"JSON Beta","url":"/detail/beta"}],"next":null}`

func newLocalSite() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
		case "/list":
			_, _ = io.WriteString(w, `<article class="item"><a href="/detail/alpha">Alpha</a></article>`)
		case "/detail/alpha":
			_, _ = io.WriteString(w, `<article><h1>Alpha detail</h1><p class="summary">A public summary.</p></article>`)
		case "/render":
			_, _ = io.WriteString(w, `<html><body><div id="mount"></div><script>setTimeout(()=>{document.getElementById('mount').innerHTML='<section class="card"><h2>Rendered Alpha</h2></section>'},800);</script></body></html>`)
		case "/json":
			_, _ = io.WriteString(w, `<html><body><div id="api-result"></div><script>fetch('/api/items?page=1').then(r=>r.json()).then(data=>document.getElementById('api-result').textContent=data.items[0].title);</script></body></html>`)
		case "/api/items":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				_, _ = io.WriteString(w, jsonPageTwo)
			} else {
				_, _ = io.WriteString(w, jsonPageOne)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func replaySteps(base string, fixtures []stageFixture, render bool) []model.ReplayStep {
	var steps []model.ReplayStep
	add := func(action map[string]any) {
		body, _ := json.Marshal(action)
		steps = append(steps, model.ReplayStep{Reply: model.Reply{Content: string(body)}})
	}
	for _, fixture := range fixtures {
		if fixture.goal.Stage == "json" && render {
			add(map[string]any{"tool": "record_api", "url": base + "/json"})
		} else {
			mode := "http"
			if fixture.goal.Transport == "browser" {
				mode = "render"
			}
			action := map[string]any{"tool": "fetch_page", "url": base + fixture.seed, "mode": mode}
			if mode == "render" {
				action["wait"] = map[string]any{"selector": "section.card > h2", "state": "visible", "timeout_ms": 3000}
			}
			add(action)
		}
		add(map[string]any{"tool": "read_evidence", "evidence_id": "latest", "limit": 16384})
		if fixture.goal.Stage == "json" {
			add(map[string]any{"tool": "fetch_page", "url": base + "/api/items?page=2", "mode": "http"})
		}
		raw, _ := json.Marshal(fixture.rule)
		var rule map[string]any
		_ = json.Unmarshal(raw, &rule)
		delete(rule, "id")
		delete(rule, "version")
		add(map[string]any{"tool": "submit_candidate", "stage": fixture.goal.Stage, "rule": rule})
		add(map[string]any{"tool": "validate_candidate", "stage": fixture.goal.Stage})
	}
	add(map[string]any{"tool": "finish", "note": "Host-authored replay completed; run independent checks and retain candidates for review."})
	return steps
}
