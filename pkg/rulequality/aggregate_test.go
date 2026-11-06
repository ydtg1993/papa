package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func metricHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func metricWindow(n int) Window {
	start := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	scope := Scope{Site: "test", Stage: "detail", RuleID: "detail", Version: "v1"}
	w := Window{Query: Query{Scope: scope, From: start, Until: start.Add(time.Hour), Limit: MaxObservations}, Cutoff: uint64(n)}
	for i := 0; i < n; i++ {
		w.Observations = append(w.Observations, Observation{ID: fmt.Sprintf("observation-%03d", i), Sequence: uint64(i + 1), CreatedAt: start.Add(time.Duration(i+1) * time.Second), Scope: scope, RequestSHA256: metricHash(fmt.Sprint(i % 3)), FieldSchemaSHA256: metricHash("schema"), CompleteResponse: true, HTTPStatus: 200, Evaluated: true, ExtractionOK: true, RawRows: 1, NewRows: 1, Operation: "complete", Fields: []FieldStat{{Name: "title", Kind: "string", Rows: 1, Present: 1, NonEmpty: 1}}})
	}
	return w
}
func metricPolicy() Policy           { p := DefaultPolicy(); p.MinPages = 1; p.MinRequests = 1; return p }
func metricFloat(x float64) *float64 { return &x }

func TestAggregateRetriesDoNotInflateLogicalPages(t *testing.T) {
	w := metricWindow(30)
	for i := range w.Observations {
		w.Observations[i].LogicalKey = metricHash("same-page")
		w.Observations[i].ExtractionOK = false
		w.Observations[i].FailureKind = FailureValidation
	}
	w.Observations[29].ExtractionOK = true
	w.Observations[29].FailureKind = ""
	r, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != Insufficient || r.Metrics.Pages != 1 || r.Metrics.Attempts != 30 || r.Metrics.ExtractionFailures != 0 || r.Metrics.FailureCounts[FailureValidation] != 29 {
		t.Fatalf("retry folding: %+v", r)
	}
	if r.Folding == "" || r.Coverage != "recorded_observations_only" {
		t.Fatal("missing coverage/folding contract")
	}
}

func TestAggregateUpstreamAndSaveFailuresNeverImplyRuleDrift(t *testing.T) {
	for _, kind := range []string{FailureNetwork, FailureHTTP, FailureRateLimit, FailureAuthentication, FailureChallenge, FailureRender, FailureSave, FailureSubmit, FailurePlanning, FailureBudget, FailureCanceled, FailureHost, FailureRule} {
		t.Run(kind, func(t *testing.T) {
			w := metricWindow(20)
			for i := range w.Observations {
				w.Observations[i].FailureKind = kind
				w.Observations[i].ExtractionOK = false
			}
			r, err := Aggregate(w, DefaultPolicy(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Verdict == SuspectedDrift || r.Verdict == ObservedOK || r.Metrics.ExtractionFailures != 0 {
				t.Fatalf("wrong diagnosis: %+v", r)
			}
		})
	}
}

func TestAggregateInsufficientPartialAndNilThresholds(t *testing.T) {
	for _, change := range []func(*Window){func(w *Window) { w.Truncated = true }, func(w *Window) { w.Query.LostObservations = 1 }, func(w *Window) { w.Observations[0].FieldsIncomplete = true }, func(w *Window) { w.Observations[0].Truncated = true }} {
		w := metricWindow(20)
		change(&w)
		r, err := Aggregate(w, DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Verdict != Partial {
			t.Fatalf("expected partial, got %s", r.Verdict)
		}
	}
	w := metricWindow(20)
	for i := range w.Observations {
		w.Observations[i].ExtractionOK = false
		w.Observations[i].FailureKind = FailureValidation
	}
	p := DefaultPolicy()
	p.ExtractionFailureRate = nil
	r, err := Aggregate(w, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict == SuspectedDrift {
		t.Fatal("nil threshold unexpectedly enabled")
	}
	p.ExtractionFailureRate = metricFloat(math.NaN())
	if _, err := Aggregate(w, p, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nonfinite threshold=%v", err)
	}
}

func TestAggregateZeroRowsAndNoNewRowsAreNotDrift(t *testing.T) {
	for _, zero := range []bool{false, true} {
		w := metricWindow(20)
		for i := range w.Observations {
			o := &w.Observations[i]
			o.NewRows = 0
			if zero {
				o.RawRows = 0
				o.Fields = nil
				o.ExtractionOK = false
				o.FailureKind = FailureExtraction
			}
		}
		r, err := Aggregate(w, DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Verdict == SuspectedDrift {
			t.Fatalf("zero=%v was drift", zero)
		}
		if zero {
			p := DefaultPolicy()
			n := 1
			p.MinRows = &n
			r, err = Aggregate(w, p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Verdict != SuspectedDrift {
				t.Fatalf("explicit min rows was ignored: %+v", r)
			}
		}
	}
}

func TestAggregatePerFieldDenominatorsAndDetachedPolicy(t *testing.T) {
	w := metricWindow(20)
	for i := 10; i < 20; i++ {
		w.Observations[i].Fields = []FieldStat{{Name: "other", Kind: "string", Rows: 1, Present: 1, NonEmpty: 1}}
		w.Observations[i].FieldSchemaSHA256 = metricHash("other-schema")
	}
	p := DefaultPolicy()
	p.MissingRate = metricFloat(.2)
	r, err := Aggregate(w, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Metrics.Fields[0].Rows != 10 || r.Metrics.Fields[1].Rows != 10 || r.Verdict == SuspectedDrift {
		t.Fatalf("wrong denominators: %+v", r)
	}
	*p.MissingRate = .9
	w.Observations[0].Fields[0].Name = "mutated"
	if *r.Policy.MissingRate != .2 || r.Metrics.Fields[1].Name != "title" {
		t.Fatal("report aliases its inputs")
	}
}

func TestAggregateBaselineCompatibilityAndDrops(t *testing.T) {
	p := DefaultPolicy()
	p.MaxFieldDrop = metricFloat(.2)
	base, err := Aggregate(metricWindow(20), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A baseline can itself be built before selection; unavailable comparison is insufficient.
	base.Verdict = ObservedOK
	w := metricWindow(20)
	for i := range w.Observations {
		w.Observations[i].Fields[0].NonEmpty = 0
	}
	r, err := Aggregate(w, p, &base)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != SuspectedDrift {
		t.Fatalf("field drop ignored: %+v", r)
	}
	base.Query.Scope.Stage = "other"
	if _, err := Aggregate(w, p, &base); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("cross-stage baseline=%v", err)
	}
	base.Query.Scope.Stage = w.Query.Scope.Stage
	base.Query.Scope.Version = "v2"
	if _, err := Aggregate(w, p, &base); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("cross-version baseline=%v", err)
	}
	base.Query.Scope.Version = w.Query.Scope.Version
	base.Policy.MinPages++
	if _, err := Aggregate(w, p, &base); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("cross-policy baseline=%v", err)
	}
}

func TestAggregateInputDigestAndReferencesBounded(t *testing.T) {
	w := metricWindow(100)
	r, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Evidence) != MaxReferences || len(r.InputSHA256) != 64 {
		t.Fatalf("bounds/digest: %+v", r)
	}
	for a, b := 0, len(w.Observations)-1; a < b; a, b = a+1, b-1 {
		w.Observations[a], w.Observations[b] = w.Observations[b], w.Observations[a]
	}
	s, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.InputSHA256 != s.InputSHA256 {
		t.Fatal("input order changed digest")
	}
	w.Observations[0].NewRows = 0
	s, err = Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.InputSHA256 == s.InputSHA256 {
		t.Fatal("changed input retained digest")
	}
}

func TestAggregateBuildReportStoreFlow(t *testing.T) {
	store := NewMemoryStore()
	w := metricWindow(1)
	o := w.Observations[0]
	o.CreatedAt = time.Time{}
	o.Sequence = 0
	if _, err := store.Record(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	w.Query.From = time.Now().Add(-time.Minute)
	w.Query.Until = time.Now().Add(time.Minute)
	r, err := BuildReport(context.Background(), store, w.Query, metricPolicy(), "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Report(context.Background(), r.ID)
	if err != nil || stored.InputSHA256 != r.InputSHA256 {
		t.Fatalf("fixed report missing: %+v %v", stored, err)
	}
}

func TestAggregateBaselineExplicitVersionComparisonRequiresWholeSchema(t *testing.T) {
	p := metricPolicy()
	p.CompareAcrossVersions = true
	base, err := Aggregate(metricWindow(1), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := metricWindow(1)
	w.Query.Scope.Version = "v2"
	w.Observations[0].Scope.Version = "v2"
	if _, err := Aggregate(w, p, &base); err != nil {
		t.Fatalf("explicit compatible version comparison: %v", err)
	}
	w.Observations[0].FieldSchemaSHA256 = metricHash("different validation")
	if _, err := Aggregate(w, p, &base); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("changed validation schema accepted: %v", err)
	}
}

func TestAggregateSmallEvaluatedSampleCannotBeAmplifiedByNetworkFailures(t *testing.T) {
	w := metricWindow(20)
	for i := range w.Observations {
		o := &w.Observations[i]
		o.ExtractionOK = false
		if i == 0 {
			o.FailureKind = FailureValidation
			continue
		}
		o.FailureKind = FailureNetwork
		o.Evaluated = false
		o.CompleteResponse = false
		o.RawRows, o.NewRows = 0, 0
		o.Fields = nil
	}
	r, err := Aggregate(w, DefaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict == SuspectedDrift || r.Verdict == ObservedOK {
		t.Fatalf("one evaluated page became sustained evidence: %+v", r)
	}
}

func TestAggregateMissingFieldCoverageCannotClaimHealthy(t *testing.T) {
	for _, mutate := range []func(*Observation){func(o *Observation) { o.Fields = nil }, func(o *Observation) { o.Fields[0].Rows = 0; o.Fields[0].Present = 0; o.Fields[0].NonEmpty = 0 }, func(o *Observation) { o.Fields[0].Name = "different" }} {
		w := metricWindow(20)
		mutate(&w.Observations[0])
		r, err := Aggregate(w, DefaultPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Verdict != Partial {
			t.Fatalf("unreported field coverage: %+v", r)
		}
	}
}

func TestAggregateRejectsOverflowMixedScopeAndDuplicateIdentity(t *testing.T) {
	w := metricWindow(2)
	maximum := int(^uint(0) >> 1)
	w.Observations[0].RawRows, w.Observations[1].RawRows = maximum, maximum
	if _, err := Aggregate(w, metricPolicy(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow=%v", err)
	}
	w = metricWindow(2)
	w.Observations[1].Scope.Version = "other"
	if _, err := Aggregate(w, metricPolicy(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed scope=%v", err)
	}
	w = metricWindow(2)
	w.Observations[1].ID = w.Observations[0].ID
	if _, err := Aggregate(w, metricPolicy(), nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate identity=%v", err)
	}
}

func TestAggregateBaselineWithoutEvaluatedSchemaKeepsDiagnosticReport(t *testing.T) {
	p := DefaultPolicy()
	baseline, err := Aggregate(metricWindow(20), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 20} {
		w := metricWindow(size)
		for i := range w.Observations {
			o := &w.Observations[i]
			o.Evaluated, o.ExtractionOK, o.CompleteResponse = false, false, false
			o.RawRows, o.NewRows = 0, 0
			o.Fields, o.FieldSchemaSHA256 = nil, ""
			o.FailureKind = FailureNetwork
		}
		report, err := Aggregate(w, p, &baseline)
		if err != nil {
			t.Fatalf("size=%d dropped useful diagnosis: %v", size, err)
		}
		if report.Verdict != Partial && report.Verdict != Insufficient {
			t.Fatalf("size=%d unsafe verdict: %s", size, report.Verdict)
		}
		if report.Metrics.FailureCounts[FailureNetwork] != size || report.BaselineID != baseline.ID {
			t.Fatalf("diagnosis metadata lost: %+v", report)
		}
		comparisonUnavailable := false
		for _, signal := range report.Signals {
			if signal.Code == "comparison_unavailable" {
				comparisonUnavailable = true
			}
		}
		if !comparisonUnavailable {
			t.Fatal("missing explicit unavailable comparison")
		}
		if err := validateReport(report); err != nil {
			t.Fatalf("unpersistable diagnostic: %v", err)
		}
		wrong := baseline
		wrong.Query.Scope.Site = "wrong"
		if _, err := Aggregate(w, p, &wrong); !errors.Is(err, ErrIncompatible) {
			t.Fatalf("unknown schema hid incompatible site: %v", err)
		}
		wrong = baseline
		wrong.Policy.MinPages++
		if _, err := Aggregate(w, p, &wrong); !errors.Is(err, ErrIncompatible) {
			t.Fatalf("unknown schema hid incompatible policy: %v", err)
		}
	}
}
