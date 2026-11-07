// rulepublish demonstrates durable publication using a caller-owned, dedicated
// MySQL database and a loopback HTTP site. It never contacts production sites.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dir := flag.String("sample-dir", "", "directory for retained public fixture bodies")
	migrate := flag.Bool("migrate", false, "explicitly create framework tables in the dedicated example database")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := command(ctx, os.Stdout, *dir, *migrate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, output io.Writer, dir string, migrate bool) error {
	dsn := os.Getenv("PAPA_RULE_DSN")
	if dsn == "" || dir == "" {
		return fmt.Errorf("set PAPA_RULE_DSN to a dedicated MySQL database and supply -sample-dir")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("connect dedicated MySQL database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if migrate {
		cfg := &config.Config{Rules: config.RulesConfig{Enabled: true, SampleDir: dir}}
		if err := database.Migrate(db.WithContext(ctx), cfg); err != nil {
			return fmt.Errorf("explicit migration: %w", err)
		}
	}
	return run(ctx, output, db, dir)
}

type demoResult struct {
	Site             string   `json:"site"`
	Generations      []uint64 `json:"generations"`
	VerifiedVersions int      `json:"verified_versions"`
	ChangedRulePaths int      `json:"changed_rule_paths"`
	ShadowChanged    bool     `json:"shadow_changed"`
	OldTaskTitle     string   `json:"old_task_title"`
	NewTaskTitle     string   `json:"new_task_title"`
	RollbackTitle    string   `json:"rollback_title"`
	RestoredHistory  int      `json:"restored_history"`
	ChildPinned      bool     `json:"child_pinned"`
	SampleRestored   bool     `json:"sample_restored"`
}

func run(ctx context.Context, output io.Writer, db *gorm.DB, dir string) error {
	const body = `<article><h2>Alpha</h2><h3>Beta</h3><a href="/detail">detail</a></article>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	blobs, err := rulestore.NewFileBlobs(dir)
	if err != nil {
		return err
	}
	defer blobs.Close()
	repo, err := rulestore.New(db, blobs)
	if err != nil {
		return err
	}
	prefix := "demo-" + uuid.NewString()
	actor := rulestore.Actor{Operator: "example", Source: "rulepublish loopback demo"}
	sample, err := repo.AddSample(ctx, rulestore.SampleInput{URL: srv.URL + "/page", StatusCode: 200, Body: []byte(body)}, htmlfetch.SamplePolicy{RedactBody: func(_ bool, b []byte) ([]byte, error) { return b, nil }}, actor)
	if err != nil {
		return err
	}
	bindings := func(version string) map[string]crawlrule.Reference {
		return map[string]crawlrule.Reference{"list": {ID: prefix + "-list", Version: version}, "detail": {ID: prefix + "-detail", Version: version}}
	}
	compiled := map[string]*crawlrule.Compiled{}
	for i, version := range []string{"v1", "v2"} {
		want := []string{"Alpha", "Beta"}[i]
		setID := prefix + "-" + version
		if err := repo.CreateSampleSet(ctx, setID, []rulestore.StoredFixture{{Name: "public-article", SampleID: sample.ID, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: want}}}}}, actor); err != nil {
			return err
		}
		for _, stage := range []string{"list", "detail"} {
			rule := crawlrule.Rule{SchemaVersion: 1, ID: prefix + "-" + stage, Version: version, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: []string{"h2", "h3"}[i], Validation: crawlrule.Validation{Required: true, MinLength: 1}}, {Name: "url", Selector: "a", Attribute: "href"}}}
			if _, err := repo.CreateDraft(ctx, rule, actor); err != nil {
				return err
			}
			if _, err := repo.Verify(ctx, crawlrule.Reference{ID: rule.ID, Version: version}, setID, actor); err != nil {
				return err
			}
			if stage == "list" {
				compiled[version], err = crawlrule.Compile(rule)
				if err != nil {
					return err
				}
			}
		}
	}
	first, err := repo.Publish(ctx, prefix+"-publish-1", prefix, 0, bindings("v1"), actor)
	if err != nil {
		return err
	}
	result := demoResult{Site: prefix, VerifiedVersions: 4, Generations: []uint64{first.Generation}}
	var title string
	makeFetcher := func(catalog crawlrule.Catalog) (*engine.RuleFetcher, error) {
		return engine.NewRuleFetcher(engine.RuleFetcherConfig{Site: prefix, Stage: "list", Catalog: catalog, Origins: []string{srv.URL}, Links: []engine.RuleLink{{Field: "url", Stage: "detail"}}, Save: func(_ context.Context, _ *engine.Task, page engine.RulePage) error {
			title = page.Result.Records[0]["title"].(string)
			return nil
		}})
	}
	f, err := makeFetcher(repo)
	if err != nil {
		return err
	}
	old, err := f.NewTaskContext(ctx, engine.RuleRequest{URL: srv.URL + "/page"}, nil)
	if err != nil {
		return err
	}
	second, err := repo.Publish(ctx, prefix+"-publish-2", prefix, first.Generation, bindings("v2"), actor)
	if err != nil {
		return err
	}
	result.Generations = append(result.Generations, second.Generation)
	newer, err := f.NewTaskContext(ctx, engine.RuleRequest{URL: srv.URL + "/page"}, nil)
	if err != nil {
		return err
	}
	diff, err := rulestore.CompareRules(compiled["v1"], compiled["v2"])
	if err != nil {
		return err
	}
	result.ChangedRulePaths = len(diff.Changes)
	response, err := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}).Do(ctx, htmlfetch.Request{URL: srv.URL + "/page"})
	if err != nil {
		return err
	}
	shadow, err := rulestore.Shadow(compiled["v1"], compiled["v2"], rulestore.ShadowInput{Response: response})
	if err != nil {
		return err
	}
	result.ShadowChanged = shadow.ResultChanged
	// Reopen both the body directory and repository: nothing is re-registered or
	// reactivated in memory. Production also recreates its database connection.
	reopened, err := rulestore.NewFileBlobs(dir)
	if err != nil {
		return err
	}
	defer reopened.Close()
	restored, err := rulestore.New(db.Session(&gorm.Session{}), reopened)
	if err != nil {
		return err
	}
	retained, err := reopened.Get(ctx, sample.BodySHA256, rulestore.MaxSampleBodyBytes)
	if err != nil {
		return err
	}
	result.SampleRestored = string(retained) == body
	f, err = makeFetcher(restored)
	if err != nil {
		return err
	}
	runTask := func(task *engine.Task) error {
		return f.RunHTTP(ctx, task, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}), func(child *engine.Task) error {
			if child.Meta["rule_version"] == task.Meta["rule_version"] {
				result.ChildPinned = true
			} else {
				return fmt.Errorf("child version changed during publication")
			}
			return nil
		})
	}
	if err := runTask(old); err != nil {
		return err
	}
	result.OldTaskTitle = title
	if err := runTask(newer); err != nil {
		return err
	}
	result.NewTaskTitle = title
	rolled, err := restored.Rollback(ctx, prefix+"-rollback", prefix, second.Generation, actor)
	if err != nil {
		return err
	}
	result.Generations = append(result.Generations, rolled.Generation)
	after, err := f.NewTaskContext(ctx, engine.RuleRequest{URL: srv.URL + "/page"}, nil)
	if err != nil {
		return err
	}
	if err := runTask(after); err != nil {
		return err
	}
	result.RollbackTitle = title
	history, err := restored.History(ctx, prefix, 10)
	if err != nil {
		return err
	}
	result.RestoredHistory = len(history)
	if result.OldTaskTitle != "Alpha" || result.NewTaskTitle != "Beta" || result.RollbackTitle != "Alpha" || !result.ShadowChanged || !result.ChildPinned || !result.SampleRestored || result.RestoredHistory != 3 {
		return fmt.Errorf("publication demo invariants failed")
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
