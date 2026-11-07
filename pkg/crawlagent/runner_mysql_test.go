package crawlagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func runnerMySQL(t *testing.T) (*gorm.DB, *rulestore.Repository, *rulestore.FileBlobs, *agentstore.MySQLStore, string, string) {
	t.Helper()
	dsn := os.Getenv("PAPA_AGENT_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_AGENT_MYSQL_TEST_DSN not set; real runner persistence integration not run")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_agent_test" {
		t.Fatal("runner integration requires dedicated papa_agent_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// Explicit harness migration. Agent.New/Run and agentstore.New do no DDL.
	if err := db.AutoMigrate(&models.AgentRun{}, &models.AgentStep{}, &models.RuleVersion{}, &models.RuleSample{}, &models.RuleSampleSet{}, &models.RuleVerification{}, &models.RuleRelease{}, &models.RuleSite{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	blobs, err := rulestore.NewFileBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	repo, err := rulestore.New(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := agentstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, repo, blobs, audit, "runner-" + uuid.NewString(), dir
}

func createRunnerMySQLFixture(t *testing.T, repo *rulestore.Repository, setID, fixtureURL, value string) {
	t.Helper()
	s, err := repo.AddSample(context.Background(), rulestore.SampleInput{URL: fixtureURL, StatusCode: 200, Body: []byte("<article><b>" + value + "</b></article>")}, runnerPolicy, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.CreateSampleSet(context.Background(), setID, []rulestore.StoredFixture{{Name: "fixed-" + setID, SampleID: s.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Row: 0, Pointer: "/title", Value: value}}}}}, runnerActor)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunnerMySQLFullLoopReopenLeavesPublicationUntouched(t *testing.T) {
	db, repo, blobs, audit, prefix, dir := runnerMySQL(t)
	var robotsHits, pageHits atomic.Int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			robotsHits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nDisallow:\n"))
		case "/list":
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<article><b>FETCH_DB_BODY</b></article>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	createRunnerMySQLFixture(t, repo, prefix+"-training", site.URL+"/training", "TRAIN_DB_BODY")
	createRunnerMySQLFixture(t, repo, prefix+"-holdout", site.URL+"/hidden", "HOLDOUT_DB_BODY")
	replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`, candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`, `{"tool":"finish"}`)
	cfg := crawlagent.Config{Model: replay, ModelName: "deterministic-mysql-replay", Audit: audit, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: site.URL, AllowPrivateNetwork: true}}}}, Budgets: crawlagent.Budgets{MaxDuration: 5 * time.Second}}
	goal := crawlagent.Goal{Site: prefix, Seeds: []string{site.URL + "/list"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-catalog", TrainingSet: prefix + "-training", HoldoutSet: prefix + "-holdout", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
	result, err := runAgent(t, cfg, goal)
	if err != nil || !result.Verified || result.Status != agentstore.AwaitingReview || len(result.Candidates) != 1 {
		t.Fatalf("full loop: %+v %v", result, err)
	}
	if robotsHits.Load() != 1 || pageHits.Load() != 1 || result.Usage.Requests != 2 || result.Usage.ModelCalls != 4 {
		t.Fatalf("actual request/model counts robots=%d pages=%d usage=%+v", robotsHits.Load(), pageHits.Load(), result.Usage)
	}
	// A new DB connection and filesystem handle must recover the same records.
	reopenDB, err := gorm.Open(mysql.Open(os.Getenv("PAPA_AGENT_MYSQL_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	reopenSQL, _ := reopenDB.DB()
	defer reopenSQL.Close()
	reopenBlobs, err := rulestore.NewFileBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenBlobs.Close()
	reopenRepo, err := rulestore.New(reopenDB, reopenBlobs)
	if err != nil {
		t.Fatal(err)
	}
	reopenAudit, err := agentstore.New(reopenDB)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reopenAudit.Get(context.Background(), result.RunID)
	if err != nil || run.Status != agentstore.AwaitingReview || run.FinishedAt == nil {
		t.Fatalf("reopened audit: %+v %v", run, err)
	}
	var persisted crawlagent.Result
	if err := json.Unmarshal([]byte(run.ResultJSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.Verified || !reflect.DeepEqual(persisted.Candidates, result.Candidates) || !reflect.DeepEqual(persisted.Usage, result.Usage) {
		t.Fatalf("lost final result: %+v", persisted)
	}
	steps, err := reopenAudit.Steps(context.Background(), result.RunID, 0)
	if err != nil || len(steps) != 8 {
		t.Fatalf("durable steps: %d %v", len(steps), err)
	}
	for _, step := range steps {
		if step.Status != agentstore.StepSucceeded || step.FinishedAt == nil {
			t.Fatalf("unfinished durable step: %+v", step)
		}
	}
	version, err := reopenRepo.Get(context.Background(), result.Candidates[0].Reference)
	if err != nil || version.EverPublished || version.State != "verified" {
		t.Fatalf("candidate publication state: %+v %v", version, err)
	}
	for _, id := range []string{result.Candidates[0].TrainingReportID, result.Candidates[0].HoldoutReportID} {
		var report models.RuleVerification
		if err := reopenDB.Where("id = ?", id).Take(&report).Error; err != nil || !report.Passed {
			t.Fatalf("durable report %s: %+v %v", id, report, err)
		}
	}
	for _, model := range []any{&models.RuleRelease{}, &models.RuleSite{}} {
		var count int64
		if err := db.Model(model).Where("site = ?", prefix).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("runner touched publication metadata: %d %v", count, err)
		}
	}
	for _, secret := range []string{"HOLDOUT_DB_BODY", prefix + "-holdout"} {
		if strings.Contains(runnerJSON(replay.Calls()), secret) {
			t.Fatalf("hidden fixture leaked: %s", secret)
		}
	}
	for _, body := range []string{"TRAIN_DB_BODY", "HOLDOUT_DB_BODY", "FETCH_DB_BODY"} {
		if strings.Contains(run.SnapshotJSON+run.ResultJSON+runnerJSON(steps), body) {
			t.Fatalf("body duplicated into audit: %s", body)
		}
	}
}

func TestRunnerMySQLUnconfirmedAuditRequiresExplicitInterrupt(t *testing.T) {
	_, repo, blobs, audit, prefix, _ := runnerMySQL(t)
	replay := replayActions(`{"tool":"fetch_page","url":"https://site.example/list"}`)
	source := &runnerSource{body: "<article><b>unused</b></article>"}
	cfg := crawlagent.Config{Model: replay, ModelName: "interrupted-mysql-replay", Audit: &failingAudit{Store: audit, endAt: 1}, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://site.example"}}}}, SourceFactory: func(c webaccess.Config) (crawlagent.Source, error) { source.config = c; return source, nil }}
	goal := crawlagent.Goal{Site: prefix, Seeds: []string{"https://site.example/list"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-catalog", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
	result, err := runAgent(t, cfg, goal)
	if !errors.Is(err, crawlagent.ErrAudit) || result.Status != agentstore.Interrupted || len(replay.Calls()) != 1 || source.calls.Load() != 0 || source.closed.Load() != 1 {
		t.Fatalf("audit uncertainty: %+v %v", result, err)
	}
	reopenedDB, err := gorm.Open(mysql.Open(os.Getenv("PAPA_AGENT_MYSQL_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := reopenedDB.DB()
	defer sqlDB.Close()
	reopened, err := agentstore.New(reopenedDB)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reopened.Get(context.Background(), result.RunID)
	if err != nil || run.Status != agentstore.Running || run.Revision != 2 {
		t.Fatalf("unconfirmed state lost: %+v %v", run, err)
	}
	steps, err := reopened.Steps(context.Background(), run.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != agentstore.StepPending {
		t.Fatalf("unconfirmed step: %+v %v", steps, err)
	}
	// Run has returned and this host has established that its worker ended.
	if _, err := reopened.Interrupt(context.Background(), run.ID, run.Revision); err != nil {
		t.Fatal(err)
	}
	steps, err = reopened.Steps(context.Background(), run.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != agentstore.StepUnknown || len(replay.Calls()) != 1 {
		t.Fatalf("recovery replayed model: %+v %v", steps, err)
	}
}
