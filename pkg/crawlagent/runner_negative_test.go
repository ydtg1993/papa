package crawlagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

func TestRunnerMixedFixturesKeepHiddenExpectationsAndManualPublication(t *testing.T) {
	replay := replayActions(candidateAction("b"), `{"tool":"validate_candidate","stage":"catalog"}`, `{"tool":"finish"}`)
	cfg, goal, repo, source, _ := runnerHarness(t, replay)
	addRunnerFixture(t, repo, "train", "https://site.example/train", "<article><b>TRAIN</b></article>", "TRAIN")
	addRunnerFixture(t, repo, "HIDDEN_SET", "https://site.example/hidden", "<article><b>HIDDEN_POSITIVE</b></article>", "HIDDEN_POSITIVE")
	for _, id := range []string{"train", "HIDDEN_SET"} {
		sample, err := repo.AddSample(context.Background(), rulestore.SampleInput{URL: "https://site.example/" + id + "-negative", StatusCode: 200, Body: []byte("<main>" + id + "-PRIVATE_NEGATIVE_EXPECTATION</main>")}, runnerPolicy, runnerActor)
		if err != nil {
			t.Fatal(err)
		}
		set := repo.sets[id]
		set.Fixtures = append(set.Fixtures, rulestore.StoredFixture{Name: id + "-negative", SampleID: sample.ID, Expect: crawlrule.Expectations{Kind: "empty"}})
		repo.sets[id] = set
	}
	goal.Stages[0].TrainingSet, goal.Stages[0].HoldoutSet = "train", "HIDDEN_SET"
	before := runnerJSON(repo.sets)
	result, err := runAgent(t, cfg, goal)
	if err != nil || !result.Verified || result.Status != agentstore.AwaitingReview || source.calls.Load() != 0 {
		t.Fatalf("mixed verification: %+v %v", result, err)
	}
	if before != runnerJSON(repo.sets) {
		t.Fatal("agent modified host expectations")
	}
	for _, proof := range repo.verifications {
		if !proof.Passed || proof.Report.Positive != 1 || proof.Report.Negative != 1 {
			t.Fatalf("mixed report: %+v", proof)
		}
	}
	transcript := runnerJSON(replay.Calls())
	if strings.Contains(transcript, "HIDDEN_SET") || strings.Contains(transcript, "HIDDEN_POSITIVE") {
		t.Fatal("negative/positive holdout disclosed")
	}
	if !strings.Contains(transcript, `\"kind\":\"empty\"`) {
		t.Fatal("training kind missing from correction report")
	}
	for _, v := range repo.versions {
		if v.EverPublished {
			t.Fatal("agent published negative-fixture candidate")
		}
	}
}

func TestRunnerRejectsNegativeOnlySetsBeforeModel(t *testing.T) {
	for _, holdout := range []bool{false, true} {
		replay := replayActions(candidateAction("b"))
		cfg, goal, repo, source, _ := runnerHarness(t, replay)
		addRunnerFixture(t, repo, "set", "https://site.example/sample", "<main>empty</main>", "unused")
		set := repo.sets["set"]
		set.Fixtures[0].Expect = crawlrule.Expectations{Kind: "empty"}
		repo.sets["set"] = set
		if holdout {
			goal.Stages[0].HoldoutSet = "set"
		} else {
			goal.Stages[0].TrainingSet = "set"
		}
		result, err := runAgent(t, cfg, goal)
		if err == nil || len(replay.Calls()) != 0 || source.calls.Load() != 0 || result.Verified {
			t.Fatalf("negative-only input consumed model/network: %+v %v", result, err)
		}
	}
}
