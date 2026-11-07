package crawlagent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func TestRunnerMySQLNegativeFixturesReopenWithoutPublication(t *testing.T) {
	db, repo, blobs, audit, prefix, _ := runnerMySQL(t)
	ctx := context.Background()
	for _, kind := range []string{"train", "holdout"} {
		id := prefix + "-" + kind
		createRunnerMySQLFixture(t, repo, id, "https://site.example/"+kind, kind+"-positive")
		set, err := repo.SampleSet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, neg := range []struct {
			name   string
			status int
			expect crawlrule.Expectations
		}{{"empty", 200, crawlrule.Expectations{Kind: "empty"}}, {"denied", 403, crawlrule.Expectations{Kind: "reject", ErrorKind: "http_status", StatusCode: 403}}} {
			sample, err := repo.AddSample(ctx, rulestore.SampleInput{URL: "https://site.example/" + kind + "/" + neg.name, StatusCode: neg.status, Body: []byte("<main>" + kind + "-" + neg.name + "</main>")}, runnerPolicy, runnerActor)
			if err != nil {
				t.Fatal(err)
			}
			set.Fixtures = append(set.Fixtures, rulestore.StoredFixture{Name: neg.name, SampleID: sample.ID, Expect: neg.expect})
		}
		if err := repo.CreateSampleSet(ctx, id+"-mixed", set.Fixtures, runnerActor); err != nil {
			t.Fatal(err)
		}
	}
	source := &runnerSource{}
	cfg := crawlagent.Config{Model: replayActions(candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`, `{"tool":"finish"}`), ModelName: "negative-fixture-replay", Audit: audit, Repository: repo, Blobs: blobs, Actor: runnerActor, SamplePolicy: runnerPolicy,
		Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://site.example"}}}}, SourceFactory: func(c webaccess.Config) (crawlagent.Source, error) { source.config = c; return source, nil }}
	goal := crawlagent.Goal{Site: prefix, Seeds: []string{"https://site.example/list"}, Stages: []crawlagent.StageGoal{{Stage: "catalog", RuleID: prefix + "-catalog", TrainingSet: prefix + "-train-mixed", HoldoutSet: prefix + "-holdout-mixed", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}}}}
	result, err := runAgent(t, cfg, goal)
	if err != nil || !result.Verified || result.Status != agentstore.AwaitingReview || source.calls.Load() != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	reopened, err := agentstore.New(db)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reopened.Get(ctx, result.RunID)
	if err != nil || run.Status != agentstore.AwaitingReview {
		t.Fatalf("audit reopen=%+v %v", run, err)
	}
	for _, id := range []string{result.Candidates[0].TrainingReportID, result.Candidates[0].HoldoutReportID} {
		proof, err := repo.Verification(ctx, id)
		if err != nil || !proof.Passed || proof.Report.Positive != 1 || proof.Report.Negative != 2 {
			t.Fatalf("reopened proof=%+v %v", proof, err)
		}
	}
	if _, err := repo.Current(ctx, prefix); !errors.Is(err, rulestore.ErrNoRelease) {
		t.Fatalf("agent changed Current: %v", err)
	}
}
