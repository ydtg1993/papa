package main

import (
	"context"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
	"testing"
)

func TestDemoQualityUsesRealAggregation(t *testing.T) {
	d := newDemoQuality()
	if d.report.Verdict != rulequality.SuspectedDrift || d.report.Metrics.Attempts != 24 || d.report.Metrics.ExtractionFailures != 6 {
		t.Fatalf("demo report: %+v", d.report)
	}
	r, err := d.Summary(context.Background(), d.report.Query)
	if err != nil || r.ID != "" || r.InputSHA256 != d.report.InputSHA256 {
		t.Fatalf("demo summary: %+v %v", r, err)
	}
	steps, err := demoQualityTrace(102)
	if err != nil || len(steps) != 1 || steps[0].Attempt != 2 {
		t.Fatalf("demo Trace: %+v %v", steps, err)
	}
}
