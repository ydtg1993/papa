package agentadmin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"testing"
)

type reviewRules struct {
	detail  rulestore.VersionDetail
	proof   rulestore.Verification
	release rulestore.ReleasePage
}

func (r *reviewRules) Inspect(context.Context, rulestore.Reference) (rulestore.VersionDetail, error) {
	return r.detail, nil
}
func (r *reviewRules) Verification(context.Context, string) (rulestore.Verification, error) {
	return r.proof, nil
}
func (r *reviewRules) Releases(context.Context, string, uint64) (rulestore.ReleasePage, error) {
	return r.release, nil
}
func TestCandidateReviewPinsProofAndHandoffPreservesOtherStages(t *testing.T) {
	ctx := context.Background()
	audit := agentstore.NewMemoryStore()
	ref := rulestore.Reference{ID: "rule", Version: "v2"}
	proof := rulestore.Verification{ID: "proof", Reference: ref, RuleDigest: "digest", SetID: "holdout", SetDigest: "set-sha", Passed: true}
	rules := &reviewRules{proof: proof, detail: rulestore.VersionDetail{Version: rulestore.Version{Reference: ref, Digest: "digest"}, Verification: &proof}, release: rulestore.ReleasePage{Current: &rulestore.ReleaseView{Site: "site", Generation: "9007199254740993", Bindings: map[string]rulestore.Reference{"list": {ID: "rule", Version: "v1"}, "detail": {ID: "other", Version: "v4"}}}}}
	plan := crawlagent.Plan{Goal: crawlagent.Goal{Site: "site", Stages: []crawlagent.StageGoal{{Stage: "list", RuleID: "rule", HoldoutSet: "holdout"}}}}
	snapshot, _ := json.Marshal(plan)
	run, e := audit.Start(ctx, agentstore.Run{ID: "review", SnapshotJSON: string(snapshot)})
	if e != nil {
		t.Fatal(e)
	}
	result, _ := json.Marshal(crawlagent.Result{RunID: run.ID, Status: agentstore.AwaitingReview, Verified: true, Candidates: []crawlagent.Candidate{{Stage: "list", Reference: ref, HoldoutReportID: "proof", HoldoutPassed: true}}})
	if _, e = audit.Finish(ctx, run.ID, run.Revision, agentstore.AwaitingReview, string(result), `{}`); e != nil {
		t.Fatal(e)
	}
	s, e := New(Config{Audit: audit, Rules: rules})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	v, e := s.Review(ctx, run.ID, ref)
	if e != nil || !v.Publishable {
		t.Fatalf("review %+v %v", v, e)
	}
	p, e := s.Publication(ctx, run.ID, ref)
	if e != nil || len(p.Bindings) != 2 || p.Bindings["detail"].Version != "v4" || p.Generation != "9007199254740993" {
		t.Fatalf("handoff %+v %v", p, e)
	}
	p.Bindings["detail"] = crawlrule.Reference{}
	if rules.release.Current.Bindings["detail"].ID != "other" {
		t.Fatal("aliased current release")
	}
	rules.proof.RuleDigest = "changed"
	if _, e = s.Review(ctx, run.ID, ref); !errors.Is(e, rulestore.ErrCorrupt) {
		t.Fatal(e)
	}
	rules.proof = proof
	rules.proof.SetID = "training"
	if _, e = s.Review(ctx, run.ID, ref); !errors.Is(e, rulestore.ErrCorrupt) {
		t.Fatal(e)
	}
	rules.proof = proof
	latest := proof
	latest.ID = "later-proof"
	rules.detail.Verification = &latest
	v, e = s.Review(ctx, run.ID, ref)
	if e != nil || v.Publishable || v.BlockReason != "latest_verification_changed" {
		t.Fatal("latest verification ignored")
	}
	if _, e = s.Publication(ctx, run.ID, ref); !errors.Is(e, rulestore.ErrUnverified) {
		t.Fatal(e)
	}
	rules.detail.Verification = &proof
	delete(rules.release.Current.Bindings, "list")
	if _, e = s.Publication(ctx, run.ID, ref); !errors.Is(e, ErrReviewChanged) {
		t.Fatal(e)
	}
}
