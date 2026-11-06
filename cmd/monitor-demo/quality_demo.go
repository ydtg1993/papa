package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

// Immutable simulated observations. Never visits a site, DB, or model service.
type demoQuality struct {
	observations []rulequality.Observation
	report       rulequality.Report
}

func demoQualityTrace(id int) ([]core.TraceStep, error) {
	if id != 102 {
		return []core.TraceStep{}, nil
	}
	return []core.TraceStep{{Attempt: 2, Seq: 1, Step: "模拟规则质量观测", Status: "warn", Kind: "validation", Message: "演示数据：字段校验失败；没有访问真实站点或保存页面正文。", Data: `{"observation_id":"demo-observation-23","evidence_id":"demo-evidence-23","rule_id":"demo-quality-simulated","version":"v2"}`, CreatedAt: time.Now().UTC()}}, nil
}

func newDemoQuality() *demoQuality {
	d := &demoQuality{}
	now := time.Now().UTC().Add(-time.Hour)
	scope := rulequality.Scope{Site: "shop", Stage: "catalog", RuleID: "demo-quality-simulated", Version: "v2", ReleaseID: "demo-release", ReleaseGeneration: 9007199254740993}
	for i := 0; i < 24; i++ {
		o := rulequality.Observation{ID: fmt.Sprintf("demo-observation-%02d", i), Sequence: uint64(i + 1), CreatedAt: now.Add(time.Duration(i) * time.Second), Scope: scope, TaskID: 102, Attempt: i % 3, RequestSHA256: fmt.Sprintf("%064x", i+1), FieldSchemaSHA256: strings.Repeat("a", 64), CompleteResponse: true, HTTPStatus: 200, Evaluated: true, ExtractionOK: true, RawRows: 2, NewRows: 1, Fields: []rulequality.FieldStat{{Name: "title", Kind: "string", Required: true, Rows: 2, Present: 2, NonEmpty: 2}}, Operation: "complete", EvidenceID: fmt.Sprintf("demo-evidence-%02d", i), BodySHA256: strings.Repeat("b", 64)}
		if i >= 18 {
			o.ExtractionOK = false
			o.FailureKind = rulequality.FailureValidation
			o.InvalidRows = 1
			o.Fields[0].Invalid = 1
		}
		d.observations = append(d.observations, o)
	}
	r, err := rulequality.Aggregate(rulequality.Window{Query: rulequality.Query{Scope: scope, From: now.Add(-time.Minute), Until: now.Add(time.Hour), Limit: 1000}, Cutoff: 24, Observations: d.observations}, rulequality.DefaultPolicy(), nil)
	if err != nil {
		panic(err)
	}
	r.ID = "demo-quality-report-simulated"
	r.CreatedAt = now.Add(time.Minute)
	d.report = r
	return d
}
func demoQualityMatch(f rulequality.AdminFilter, s rulequality.Scope, t time.Time) bool {
	return (f.Site == "" || f.Site == s.Site) && (f.Stage == "" || f.Stage == s.Stage) && (f.RuleID == "" || f.RuleID == s.RuleID) && (f.Version == "" || f.Version == s.Version) && !t.Before(f.From) && t.Before(f.Until)
}
func (d *demoQuality) Observations(ctx context.Context, f rulequality.AdminFilter) (rulequality.ObservationPage, error) {
	p := rulequality.ObservationPage{Items: []rulequality.Observation{}, Cutoff: 24}
	for i := len(d.observations) - 1; i >= 0; i-- {
		o := d.observations[i]
		p.Scanned++
		if demoQualityMatch(f, o.Scope, o.CreatedAt) {
			p.Items = append(p.Items, o)
		}
	}
	return p, ctx.Err()
}
func (d *demoQuality) Reports(ctx context.Context, f rulequality.AdminFilter) (rulequality.ReportPage, error) {
	p := rulequality.ReportPage{Items: []rulequality.Report{}, Scanned: 1}
	if demoQualityMatch(f, d.report.Query.Scope, d.report.CreatedAt) {
		p.Items = append(p.Items, d.report)
	}
	return p, ctx.Err()
}
func (d *demoQuality) Observation(ctx context.Context, id, reportID string) (rulequality.Observation, error) {
	if reportID != "" && reportID != d.report.ID {
		return rulequality.Observation{}, rulequality.ErrEvidence
	}
	for _, o := range d.observations {
		if o.ID == id {
			return o, ctx.Err()
		}
	}
	return rulequality.Observation{}, rulequality.ErrNotFound
}
func (d *demoQuality) Report(ctx context.Context, id string) (rulequality.Report, error) {
	if id != d.report.ID {
		return rulequality.Report{}, rulequality.ErrNotFound
	}
	return d.report, ctx.Err()
}
func (d *demoQuality) Summary(ctx context.Context, q rulequality.Query) (rulequality.Report, error) {
	var observations []rulequality.Observation
	for _, o := range d.observations {
		if o.Scope == q.Scope && !o.CreatedAt.Before(q.From) && o.CreatedAt.Before(q.Until) {
			observations = append(observations, o)
		}
	}
	r, err := rulequality.Aggregate(rulequality.Window{Query: q, Cutoff: 24, Observations: observations}, rulequality.DefaultPolicy(), nil)
	r.ID = ""
	return r, err
}
