package main

import (
	"context"
	"github.com/ydtg1993/papa/v3/pkg/agentadmin"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
	"testing"
)

func TestDemoAgentFreezePrepareReviewAndManualHandoff(t *testing.T) {
	ctx := context.Background()
	q := newDemoQuality()
	rules := &demoRules{}
	a := newDemoAgent(q, rules)
	defer a.Close(ctx)
	summary, e := q.Summary(ctx, q.report.Query)
	if e != nil {
		t.Fatal(e)
	}
	report, e := q.Freeze(ctx, rulequality.FreezeInput{OperationID: "freeze", Query: summary.Query, InputSHA256: summary.InputSHA256}, "demo")
	if e != nil {
		t.Fatal(e)
	}
	sel := agentadmin.Selection{ReportID: report.ID, ProfileID: "demo", EvidenceIDs: []string{report.Evidence[0].EvidenceID}}
	p, e := a.Preview(ctx, sel, "demo")
	if e != nil {
		t.Fatal(e)
	}
	prepare := agentadmin.PrepareInput{Selection: sel, Digest: p.Digest, OperationID: "prepare"}
	r, e := a.Prepare(ctx, prepare, "demo")
	if e != nil {
		t.Fatal(e)
	}
	again, e := a.Prepare(ctx, prepare, "demo")
	if e != nil || again.ID != r.ID {
		t.Fatal(e)
	}
	r, e = a.Start(ctx, agentadmin.StartInput{Selection: sel, Digest: p.Digest, ID: r.ID, Revision: r.Revision}, "demo")
	if e != nil {
		t.Fatal(e)
	}
	run, e := a.Run(ctx, r.RunID, 0)
	if e != nil || len(run.Candidates) != 1 {
		t.Fatal(e)
	}
	review, e := a.Review(ctx, r.RunID, run.Candidates[0].Reference)
	if e != nil || !review.Publishable {
		t.Fatalf("review %+v %v", review, e)
	}
	plan, e := a.Publication(ctx, r.RunID, review.Candidate.Reference)
	if e != nil || plan.Generation != "1" {
		t.Fatal(e)
	}
	current, e := rules.Releases(ctx, "shop", 0)
	if e != nil || current.Current.Bindings["list"].Version != "v1" {
		t.Fatal("demo unexpectedly published")
	}
}
