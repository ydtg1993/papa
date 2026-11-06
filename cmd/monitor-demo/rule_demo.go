package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

// Browser-only simulation. No database, model, remote site, or real release.
type demoRules struct {
	mu       sync.Mutex
	history  []rulestore.ReleaseView
	requests map[string]string
}

func cloneDemoRule[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func demoRuleVersion(version string) (rulestore.VersionDetail, error) {
	if version != "v1" && version != "v2" {
		return rulestore.VersionDetail{}, rulestore.ErrNotFound
	}
	rule := crawlrule.Rule{SchemaVersion: 1, ID: "demo-catalog", Version: version, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "h2", Validation: crawlrule.Validation{Required: true}}}}
	if version == "v2" {
		rule.Fields[0].Selector = "h2.title"
	}
	actor := rulestore.Actor{Operator: "演示操作人", Source: "monitor-demo"}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	proof := rulestore.Verification{ID: "demo-proof-" + version, Reference: rulestore.Reference{ID: rule.ID, Version: version}, RuleDigest: "demo-digest-" + version, SetID: "demo-explicit-fixtures", SetDigest: "demo-fixtures-digest", Passed: true, VerifierVersion: rulestore.VerifierVersion, Report: crawlrule.FixtureReport{Passed: true, Results: []crawlrule.FixtureResult{{Name: "独立验收样本（模拟）", Passed: true, Rows: 2}}}, Actor: actor, CreatedAt: now}
	return rulestore.VersionDetail{Version: rulestore.Version{Reference: proof.Reference, Rule: rule, Digest: proof.RuleDigest, State: "verified", Actor: actor, CreatedAt: now}, Verification: &proof, History: []rulestore.Verification{proof}}, nil
}
func (d *demoRules) init() {
	if d.requests != nil {
		return
	}
	d.requests = map[string]string{}
	d.history = []rulestore.ReleaseView{{ID: "demo-release-1", Site: "shop", Generation: "1", Kind: "publish", OperationID: "demo-seed", Bindings: map[string]rulestore.Reference{"list": {ID: "demo-catalog", Version: "v1"}}, VerificationIDs: map[string]string{"list": "demo-proof-v1"}, Actor: rulestore.Actor{Operator: "演示种子", Source: "monitor-demo"}, CreatedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}}
}
func (d *demoRules) Versions(_ context.Context, id string, after rulestore.Reference) (rulestore.VersionList, error) {
	out := rulestore.VersionList{Items: []rulestore.VersionSummary{}}
	for _, v := range []string{"v1", "v2"} {
		if id != "" && id != "demo-catalog" {
			continue
		}
		ref := rulestore.Reference{ID: "demo-catalog", Version: v}
		if after.ID > ref.ID || (after.ID == ref.ID && after.Version >= v) {
			continue
		}
		detail, _ := demoRuleVersion(v)
		out.Items = append(out.Items, rulestore.VersionSummary{Reference: ref, Digest: detail.Version.Digest, State: "verified", CreatedAt: detail.Version.CreatedAt})
	}
	return out, nil
}
func (d *demoRules) Inspect(_ context.Context, ref rulestore.Reference) (rulestore.VersionDetail, error) {
	if ref.ID != "demo-catalog" {
		return rulestore.VersionDetail{}, rulestore.ErrNotFound
	}
	return demoRuleVersion(ref.Version)
}
func (d *demoRules) Verification(_ context.Context, id string) (rulestore.Verification, error) {
	for _, v := range []string{"v1", "v2"} {
		detail, _ := demoRuleVersion(v)
		if detail.Verification.ID == id {
			return *detail.Verification, nil
		}
	}
	return rulestore.Verification{}, rulestore.ErrNotFound
}
func (d *demoRules) current(site string) *rulestore.ReleaseView {
	for i := len(d.history) - 1; i >= 0; i-- {
		if d.history[i].Site == site {
			return &d.history[i]
		}
	}
	return nil
}
func (d *demoRules) Releases(_ context.Context, site string, before uint64) (rulestore.ReleasePage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	out := rulestore.ReleasePage{Current: d.current(site), Items: []rulestore.ReleaseView{}, Next: "0"}
	for i := len(d.history) - 1; i >= 0; i-- {
		row := d.history[i]
		g, _ := strconv.ParseUint(row.Generation, 10, 64)
		if row.Site == site && (before == 0 || g < before) {
			out.Items = append(out.Items, row)
		}
	}
	return cloneDemoRule(out), nil
}
func (d *demoRules) preview(p rulestore.RulePlan) (rulestore.RulePreview, error) {
	out := rulestore.RulePreview{Plan: p, Current: d.current(p.Site), Stages: []rulestore.StageReview{}, VerificationIDs: map[string]string{}}
	if _, err := p.Validate(); err != nil {
		return out, err
	}
	generation := "0"
	if out.Current != nil {
		generation = out.Current.Generation
	}
	if generation != p.Generation {
		return out, rulestore.ErrGenerationConflict
	}
	bindings := p.Bindings
	if p.Kind == "rollback" {
		if out.Current == nil || out.Current.PreviousReleaseID == "" {
			return out, rulestore.ErrNoRollback
		}
		for _, row := range d.history {
			if row.ID == out.Current.PreviousReleaseID {
				x := row
				out.Target = &x
				bindings = x.Bindings
			}
		}
		if out.Target == nil {
			return out, rulestore.ErrCorrupt
		}
	}
	for stage, ref := range bindings {
		detail, err := d.Inspect(context.Background(), ref)
		if err != nil {
			return out, err
		}
		item := rulestore.StageReview{Stage: stage, VersionDetail: detail}
		if out.Current != nil {
			if prior, ok := out.Current.Bindings[stage]; ok {
				old, _ := demoRuleVersion(prior.Version)
				left, _ := crawlrule.Compile(old.Version.Rule)
				right, _ := crawlrule.Compile(detail.Version.Rule)
				diff, _ := rulestore.CompareRules(left, right)
				item.Comparison = &diff
			}
		}
		out.Stages = append(out.Stages, item)
		out.VerificationIDs[stage] = detail.Verification.ID
	}
	return cloneDemoRule(out), nil
}
func (d *demoRules) Preview(_ context.Context, p rulestore.RulePlan) (rulestore.RulePreview, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	return d.preview(p)
}
func (d *demoRules) Change(_ context.Context, p rulestore.RuleChangeRequest, actor rulestore.Actor) (rulestore.ReleaseView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	if err := p.Validate(); err != nil {
		return rulestore.ReleaseView{}, err
	}
	data, _ := json.Marshal(struct {
		Request rulestore.RuleChangeRequest
		Actor   rulestore.Actor
	}{p, actor})
	fingerprint := string(data)
	if existing, ok := d.requests[p.OperationID]; ok {
		if existing != fingerprint {
			return rulestore.ReleaseView{}, rulestore.ErrOperationConflict
		}
		for _, row := range d.history {
			if row.OperationID == p.OperationID {
				return cloneDemoRule(row), nil
			}
		}
	}
	review, err := d.preview(p.RulePlan)
	if err != nil {
		return rulestore.ReleaseView{}, err
	}
	bindings := p.Bindings
	previous := ""
	if review.Current != nil {
		previous = review.Current.ID
	}
	if p.Kind == "rollback" {
		bindings = review.Target.Bindings
		previous = review.Target.PreviousReleaseID
	} else {
		for stage, proof := range review.VerificationIDs {
			if p.VerificationIDs[stage] != proof {
				return rulestore.ReleaseView{}, rulestore.ErrReviewConflict
			}
		}
	}
	generation, _ := strconv.ParseUint(p.Generation, 10, 64)
	row := rulestore.ReleaseView{ID: fmt.Sprintf("demo-release-%d", len(d.history)+1), Site: p.Site, Generation: strconv.FormatUint(generation+1, 10), Kind: p.Kind, OperationID: p.OperationID, PreviousReleaseID: previous, Bindings: bindings, VerificationIDs: review.VerificationIDs, Actor: actor, CreatedAt: time.Now()}
	row = cloneDemoRule(row)
	d.history = append(d.history, row)
	d.requests[p.OperationID] = fingerprint
	return cloneDemoRule(row), nil
}
func (d *demoRules) Operation(_ context.Context, id string) (rulestore.ReleaseView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	for _, r := range d.history {
		if r.OperationID == id {
			return cloneDemoRule(r), nil
		}
	}
	return rulestore.ReleaseView{}, rulestore.ErrNotFound
}
