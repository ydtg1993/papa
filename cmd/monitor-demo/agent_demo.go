package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v3/pkg/agentadmin"
	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
	"strconv"
	"time"
)

// UI-only simulation. It neither constructs a provider nor visits a site.
type demoAgent struct {
	*agentadmin.Service
	quality *demoQuality
	store   *rulequality.MemoryStore
	audit   *agentstore.MemoryStore
}

func newDemoAgent(q *demoQuality, r *demoRules) *demoAgent {
	audit := agentstore.NewMemoryStore()
	store := rulequality.NewMemoryStore()
	s, err := agentadmin.New(agentadmin.Config{Audit: audit, Quality: store, Rules: r})
	if err != nil {
		panic(err)
	}
	return &demoAgent{s, q, store, audit}
}
func (d *demoAgent) Capabilities() agentadmin.Capabilities {
	return agentadmin.Capabilities{Profiles: []agentadmin.ProfileView{{ID: "demo", Revision: "1", Label: "模拟分析 · 不调用模型"}}, Quality: true, Rules: true, MaxConcurrent: 1}
}
func (d *demoAgent) Preview(ctx context.Context, sel agentadmin.Selection, operator string) (agentadmin.Preview, error) {
	r, e := d.quality.Report(ctx, sel.ReportID)
	if e != nil {
		return agentadmin.Preview{}, e
	}
	if sel.ProfileID != "demo" || len(sel.EvidenceIDs) < 1 || len(sel.EvidenceIDs) > 8 {
		return agentadmin.Preview{}, agentadmin.ErrInvalid
	}
	for _, id := range sel.EvidenceIDs {
		found := false
		for _, ref := range r.Evidence {
			found = found || ref.EvidenceID == id
		}
		if !found {
			return agentadmin.Preview{}, rulequality.ErrEvidence
		}
	}
	p := agentadmin.Preview{Selection: sel, Profile: d.Capabilities().Profiles[0], Plan: crawlagent.Plan{Goal: crawlagent.Goal{Site: "shop", Stages: []crawlagent.StageGoal{{Stage: "list", RuleID: "demo-catalog", HoldoutSet: "demo-explicit-fixtures", Existing: &rulestore.Reference{ID: "demo-catalog", Version: "v1"}}}}, Model: "UI simulation / no provider", Actor: rulestore.Actor{Operator: operator, Source: "oa-agent:demo:1"}, Budgets: crawlagent.Budgets{MaxModelCalls: 4, MaxDuration: 30 * time.Second}}}
	raw, _ := json.Marshal(p)
	p.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, nil
}
func (d *demoAgent) Prepare(ctx context.Context, in agentadmin.PrepareInput, operator string) (agentadmin.RequestView, error) {
	p, e := d.Preview(ctx, in.Selection, operator)
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	if p.Digest != in.Digest {
		return agentadmin.RequestView{}, agentadmin.ErrReviewChanged
	}
	r, e := d.quality.Report(ctx, in.ReportID)
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	r.CreatedAt = time.Time{}
	if _, e = d.store.SaveReport(ctx, r); e != nil {
		return agentadmin.RequestView{}, e
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(in.OperationID)))
	request, e := d.store.CreateRequest(ctx, rulequality.Request{ID: "demo-request-" + sum, RunID: sum, OperationID: in.OperationID, ReportID: in.ReportID, Operator: operator, Source: "oa-agent:demo:1"})
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	return d.Request(ctx, request.ID)
}
func (d *demoAgent) Start(ctx context.Context, in agentadmin.StartInput, operator string) (agentadmin.RequestView, error) {
	p, e := d.Preview(ctx, in.Selection, operator)
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	if p.Digest != in.Digest {
		return agentadmin.RequestView{}, agentadmin.ErrReviewChanged
	}
	r, e := d.store.Request(ctx, in.ID)
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	if r.Operator != operator || r.ReportID != in.ReportID {
		return agentadmin.RequestView{}, agentadmin.ErrOwner
	}
	rev, e := strconv.ParseUint(in.Revision, 10, 64)
	if e != nil {
		return agentadmin.RequestView{}, agentadmin.ErrInvalid
	}
	r, e = d.store.StartRequest(ctx, r.ID, rev, p.Digest)
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	snapshot, _ := json.Marshal(p.Plan)
	run, e := d.audit.Start(ctx, agentstore.Run{ID: r.RunID, SnapshotJSON: string(snapshot)})
	if e != nil {
		return agentadmin.RequestView{}, e
	}
	result, _ := json.Marshal(crawlagent.Result{RunID: run.ID, Status: agentstore.AwaitingReview, Verified: true, Candidates: []crawlagent.Candidate{{Stage: "list", Reference: rulestore.Reference{ID: "demo-catalog", Version: "v2"}, HoldoutReportID: "demo-proof-v2", HoldoutPassed: true}}})
	if _, e = d.audit.Finish(ctx, run.ID, run.Revision, agentstore.AwaitingReview, string(result), `{}`); e != nil {
		return agentadmin.RequestView{}, e
	}
	if _, e = d.store.FinishRequest(ctx, r.ID, r.Revision, rulequality.AwaitingReview, `{"simulation":true}`); e != nil {
		return agentadmin.RequestView{}, e
	}
	return d.Request(ctx, r.ID)
}
