package main

import (
	"context"
	"errors"

	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

// This recipe identifies a synthetic, host-authored prerequisite, not a model
// answer or a publication action available to the Agent. Only training samples
// verify it. Both control and treatment install exactly the same prerequisite.
func evaluationHistoryDigest() string {
	var rules []crawlrule.Rule
	for _, s := range evaluationCorpus() {
		rules = append(rules, s.Oracle)
	}
	return evaluationDigest(struct {
		Version string
		Rules   []crawlrule.Rule
	}{"published-training-history-v1", rules})
}

func installEvaluationHistory(ctx context.Context, repo *rulestore.Repository, site, stage, trainingSet string, rule crawlrule.Rule, actor rulestore.Actor) (rulestore.Version, rulestore.Release, error) {
	rule.ID, rule.Version = site+"-history", "seed-v1"
	v, err := repo.CreateDraft(ctx, rule, actor)
	if err != nil {
		return v, rulestore.Release{}, err
	}
	proof, err := repo.Verify(ctx, v.Reference, trainingSet, actor)
	if err != nil {
		return v, rulestore.Release{}, err
	}
	release, err := repo.PublishReviewed(ctx, site+"-seed", site, 0, map[string]rulestore.Reference{stage: v.Reference}, map[string]string{stage: proof.ID}, actor)
	if err != nil {
		return v, release, err
	}
	v, err = repo.Get(ctx, v.Reference)
	return v, release, err
}

func approveEvaluationHistory(repo *rulestore.Repository, site, stage string, selected rulestore.Version) func(context.Context, crawlagent.HistoryRequest) error {
	return func(ctx context.Context, request crawlagent.HistoryRequest) error {
		if request.Site != site || request.Stage != stage || request.Version.Reference != selected.Reference || request.Version.Digest != selected.Digest {
			return errors.New("history scope differs")
		}
		current, err := repo.Current(ctx, site)
		if err != nil || current.Bindings[stage] != selected.Reference {
			return errors.New("selected history is not the approved site binding")
		}
		// Only the known authored corpus rule is approved here. Real businesses
		// must review selector/pointer literals and own their authorization policy.
		return nil
	}
}
