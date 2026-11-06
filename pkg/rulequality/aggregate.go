package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/google/uuid"
)

const reportFolding = "latest_sequence_per_logical_key_else_observation_id; attempts_counted_separately"

// DefaultPolicy enables only sustained extraction failure detection. Field and
// row expectations require explicit host policy; nil thresholds stay disabled.
func DefaultPolicy() Policy {
	rate := .2
	return Policy{MinPages: 20, MinRequests: 3, ExtractionFailureRate: &rate}
}

// BuildReport freezes a bounded Store query, optionally compares a previously
// fixed report, and persists the detached result. It performs no model calls.
func BuildReport(ctx context.Context, store Store, query Query, policy Policy, baselineID string) (Report, error) {
	if ctx == nil || collectorNil(store) {
		return Report{}, ErrInvalid
	}
	if err := validatePolicy(policy); err != nil {
		return Report{}, err
	}
	var baseline *Report
	if baselineID != "" {
		value, err := store.Report(ctx, baselineID)
		if err != nil {
			return Report{}, err
		}
		baseline = &value
	}
	window, err := store.Query(ctx, query)
	if err != nil {
		return Report{}, err
	}
	report, err := Aggregate(window, policy, baseline)
	if err != nil {
		return Report{}, err
	}
	return store.SaveReport(ctx, report)
}

// Aggregate uses the latest recorded observation of each explicit logical key.
// Callers without a logical key have independent pages, including TaskID=0.
// FailureCounts always covers all recorded attempts; extraction/field/row
// metrics cover only the folded pages that actually evaluated a complete body.
func Aggregate(window Window, policy Policy, baseline *Report) (Report, error) {
	query, err := normalizeQuery(window.Query)
	if err != nil {
		return Report{}, err
	}
	if err := validatePolicy(policy); err != nil {
		return Report{}, err
	}
	if len(window.Observations) > query.Limit {
		return Report{}, ErrInvalid
	}
	observations := append([]Observation(nil), window.Observations...)
	sort.Slice(observations, func(i, j int) bool { return aggregateEarlier(observations[i], observations[j]) })
	report := Report{
		ID: uuid.NewString(), Query: query, Cutoff: window.Cutoff,
		Policy: aggregateClonePolicy(policy), Coverage: "recorded_observations_only", Folding: reportFolding,
		Truncated: window.Truncated, Metrics: Metrics{Fields: []FieldStat{}, FailureCounts: make(map[string]int)},
	}
	latest := make(map[string]Observation)
	unique := make(map[string]Observation)
	inputs := make([]Observation, 0, len(observations))
	for _, observation := range observations {
		if err := validateObservation(observation); err != nil {
			return Report{}, err
		}
		if !aggregateScopeMatch(query.Scope, observation.Scope) || observation.CreatedAt.Before(query.From) || !observation.CreatedAt.Before(query.Until) || observation.Sequence > window.Cutoff {
			return Report{}, ErrInvalid
		}
		if previous, exists := unique[observation.ID]; exists {
			if !reflect.DeepEqual(previous, observation) {
				return Report{}, ErrConflict
			}
			continue
		}
		observation.Fields = append([]FieldStat(nil), observation.Fields...)
		unique[observation.ID] = observation
		inputs = append(inputs, observation)
		report.Metrics.Attempts++
		if observation.FailureKind != "" {
			report.Metrics.FailureCounts[observation.FailureKind]++
		}
		key := "id:" + observation.ID
		if observation.LogicalKey != "" {
			key = "logical:" + observation.LogicalKey
		}
		latest[key] = observation
	}
	// Include the cutoff and normalized query, not merely references, so changes
	// to a summary, known losses or a window cannot retain the same input digest.
	data, err := json.Marshal(struct {
		Query        Query         `json:"query"`
		Cutoff       uint64        `json:"cutoff"`
		Truncated    bool          `json:"truncated"`
		Observations []Observation `json:"observations"`
	}{query, window.Cutoff, window.Truncated, inputs})
	if err != nil {
		return Report{}, ErrInvalid
	}
	hash := sha256.Sum256(data)
	report.InputSHA256 = hex.EncodeToString(hash[:])
	pages := make([]Observation, 0, len(latest))
	for _, observation := range latest {
		pages = append(pages, observation)
	}
	sort.Slice(pages, func(i, j int) bool { return aggregateEarlier(pages[i], pages[j]) })
	requests, schemas := make(map[string]bool), make(map[string]bool)
	evaluatedRequests, schemaFields := make(map[string]bool), make(map[string]string)
	fields := make(map[string]FieldStat)
	finalFailures := make(map[string]int)
	partial, zeroUnknown := false, false
	for _, observation := range pages {
		report.Metrics.Pages++
		requests[observation.RequestSHA256] = true
		if observation.FailureKind != "" {
			finalFailures[observation.FailureKind]++
		}
		if len(report.Evidence) < MaxReferences {
			report.Evidence = append(report.Evidence, EvidenceRef{ObservationID: observation.ID, EvidenceID: observation.EvidenceID, BodySHA256: observation.BodySHA256})
		}
		if observation.FieldsIncomplete {
			partial = true
			aggregateSignal(&report, "incomplete_fields", "", 1)
		}
		if observation.Truncated {
			partial = true
			aggregateSignal(&report, "truncated_extraction", "", 1)
		}
		if !observation.Evaluated || !observation.CompleteResponse {
			continue
		}
		report.Metrics.EvaluatedPages++
		evaluatedRequests[observation.RequestSHA256] = true
		for _, count := range []struct {
			total  *int
			amount int
		}{{&report.Metrics.RawRows, observation.RawRows}, {&report.Metrics.NewRows, observation.NewRows}, {&report.Metrics.InvalidRows, observation.InvalidRows}} {
			if !aggregateAdd(count.total, count.amount) {
				return Report{}, ErrInvalid
			}
		}
		// ErrNoRecords alone is not evidence of a changed site. This also keeps
		// an empty optional result from silently enabling a minimum-row policy.
		zeroOnly := observation.RawRows == 0 && observation.InvalidRows == 0 && observation.FailureKind == FailureExtraction && observation.StopReason == "no_records"
		if !observation.ExtractionOK && zeroOnly {
			zeroUnknown = true
			aggregateSignal(&report, "zero_rows", "", 1)
		}
		if !observation.ExtractionOK && !zeroOnly && (observation.FailureKind == "" || observation.FailureKind == FailureExtraction || observation.FailureKind == FailureValidation) {
			report.Metrics.ExtractionFailures++
		}
		if observation.FieldSchemaSHA256 != "" {
			schemas[observation.FieldSchemaSHA256] = true
		} else {
			partial = true
			aggregateSignal(&report, "incomplete_fields", "", 1)
		}
		if observation.RawRows > 0 {
			fingerprint := aggregateFieldsFingerprint(observation.Fields)
			previous, exists := schemaFields[observation.FieldSchemaSHA256]
			if len(observation.Fields) == 0 || (exists && previous != fingerprint) {
				partial = true
				aggregateSignal(&report, "incomplete_fields", "", 1)
			}
			schemaFields[observation.FieldSchemaSHA256] = fingerprint
		}
		for _, field := range observation.Fields {
			if field.Rows != observation.RawRows {
				partial = true
				aggregateSignal(&report, "incomplete_fields", "", 1)
			}
			previous, exists := fields[field.Name]
			if exists && (previous.Kind != field.Kind || previous.Required != field.Required) {
				return Report{}, ErrIncompatible
			}
			previous.Name, previous.Kind, previous.Required = field.Name, field.Kind, field.Required
			for _, count := range []struct {
				total  *int
				amount int
			}{{&previous.Rows, field.Rows}, {&previous.Present, field.Present}, {&previous.Null, field.Null}, {&previous.NonEmpty, field.NonEmpty}, {&previous.Invalid, field.Invalid}} {
				if !aggregateAdd(count.total, count.amount) {
					return Report{}, ErrInvalid
				}
			}
			fields[field.Name] = previous
		}
	}
	report.Metrics.DistinctRequests = len(requests)
	for _, field := range fields {
		report.Metrics.Fields = append(report.Metrics.Fields, field)
	}
	sort.Slice(report.Metrics.Fields, func(i, j int) bool { return report.Metrics.Fields[i].Name < report.Metrics.Fields[j].Name })
	if len(report.Metrics.Fields) > MaxFields {
		report.Metrics.Fields = report.Metrics.Fields[:MaxFields]
		partial = true
		aggregateSignal(&report, "incomplete_fields", "", 1)
	}
	if len(schemas) == 1 {
		for schema := range schemas {
			report.FieldSchemaSHA256 = schema
		}
	}
	if len(schemas) > 1 {
		partial = true
		aggregateSignal(&report, "mixed_field_schema", "", len(schemas))
	}
	if report.Truncated {
		partial = true
		aggregateSignal(&report, "truncated_observations", "", 1)
	}
	if query.LostObservations > 0 {
		partial = true
		aggregateSignal(&report, "lost_observations", "", aggregateBoundUint(query.LostObservations))
	}
	if report.Metrics.Attempts > report.Metrics.Pages {
		aggregateSignal(&report, "folded_attempts", "", report.Metrics.Attempts-report.Metrics.Pages)
	}
	for kind, count := range report.Metrics.FailureCounts {
		aggregateSignal(&report, "attempt_"+kind, "", count)
	}
	comparisonAvailable := true
	if baseline != nil {
		var err error
		comparisonAvailable, err = aggregateBaselineCompatible(report, *baseline)
		if err != nil {
			return Report{}, err
		}
		report.BaselineID = baseline.ID
		if !comparisonAvailable {
			partial = true
			aggregateSignal(&report, "comparison_unavailable", "", 1)
		}
	}
	insufficient := report.Metrics.Pages < policy.MinPages || report.Metrics.DistinctRequests < policy.MinRequests
	if report.Metrics.Pages < policy.MinPages {
		aggregateSignal(&report, "sample_pages", "", report.Metrics.Pages)
	}
	if report.Metrics.DistinctRequests < policy.MinRequests {
		aggregateSignal(&report, "sample_requests", "", report.Metrics.DistinctRequests)
	}
	if baseline == nil && (policy.MaxRowsDrop != nil || policy.MaxFieldDrop != nil) {
		insufficient = true
		aggregateSignal(&report, "baseline_required", "", 1)
	}
	qualityInsufficient := report.Metrics.EvaluatedPages < policy.MinPages || len(evaluatedRequests) < policy.MinRequests
	if report.Metrics.EvaluatedPages < policy.MinPages {
		aggregateSignal(&report, "sample_evaluated_pages", "", report.Metrics.EvaluatedPages)
	}
	if len(evaluatedRequests) < policy.MinRequests {
		aggregateSignal(&report, "sample_evaluated_requests", "", len(evaluatedRequests))
	}
	drift := false
	if !qualityInsufficient && comparisonAvailable {
		drift = aggregateThresholds(&report, baseline)
	}
	upstream, processing := false, false
	for kind, count := range finalFailures {
		switch kind {
		case FailureNetwork, FailureHTTP, FailureRateLimit, FailureAuthentication, FailureChallenge, FailureRender:
			upstream = true
			aggregateSignal(&report, kind, "", count)
		case FailureSave, FailureSubmit, FailurePlanning, FailureRule, FailureBudget, FailureCanceled, FailureHost:
			processing = true
			aggregateSignal(&report, kind, "", count)
		}
	}
	switch {
	case insufficient:
		report.Verdict = Insufficient
	case partial:
		report.Verdict = Partial
	case drift:
		report.Verdict = SuspectedDrift
	case upstream:
		report.Verdict = UpstreamFailure
	case processing:
		report.Verdict = ProcessingFailure
	case qualityInsufficient:
		report.Verdict = Insufficient
	case zeroUnknown:
		report.Verdict = Partial
	default:
		report.Verdict = ObservedOK
	}
	sort.Slice(report.Signals, func(i, j int) bool {
		if report.Signals[i].Code != report.Signals[j].Code {
			return report.Signals[i].Code < report.Signals[j].Code
		}
		return report.Signals[i].Field < report.Signals[j].Field
	})
	return report, nil
}

func aggregateThresholds(report *Report, baseline *Report) bool {
	p, m := report.Policy, report.Metrics
	drift := false
	add := func(code, field string, count int) { drift = true; aggregateSignal(report, code, field, count) }
	if p.ExtractionFailureRate != nil && aggregateRatioAtLeast(m.ExtractionFailures, m.EvaluatedPages, *p.ExtractionFailureRate) {
		add("extraction_failure_rate", "", m.ExtractionFailures)
	}
	if p.MinRows != nil && m.EvaluatedPages > 0 && float64(m.RawRows)/float64(m.EvaluatedPages) < float64(*p.MinRows) {
		add("minimum_rows", "", m.RawRows)
	}
	for _, field := range m.Fields {
		if p.MissingRate != nil && aggregateRatioAtLeast(field.Rows-field.Present, field.Rows, *p.MissingRate) {
			add("missing_rate", field.Name, field.Rows-field.Present)
		}
		// Empty is distinct from missing: denominator is present values, and
		// null counts as present but empty. Zero/false remain nonempty.
		if p.EmptyRate != nil && aggregateRatioAtLeast(field.Present-field.NonEmpty, field.Present, *p.EmptyRate) {
			add("empty_rate", field.Name, field.Present-field.NonEmpty)
		}
	}
	if baseline == nil {
		return drift
	}
	if p.MaxRowsDrop != nil && m.EvaluatedPages > 0 && baseline.Metrics.EvaluatedPages > 0 {
		before := float64(baseline.Metrics.RawRows) / float64(baseline.Metrics.EvaluatedPages)
		after := float64(m.RawRows) / float64(m.EvaluatedPages)
		if aggregateDrop(before, after, *p.MaxRowsDrop) {
			add("rows_drop", "", m.RawRows)
		}
	}
	if p.MaxFieldDrop != nil {
		previous := make(map[string]FieldStat, len(baseline.Metrics.Fields))
		for _, field := range baseline.Metrics.Fields {
			previous[field.Name] = field
		}
		for _, field := range m.Fields {
			before, exists := previous[field.Name]
			if !exists || field.Rows == 0 || before.Rows == 0 {
				continue
			}
			if aggregateDrop(float64(before.NonEmpty)/float64(before.Rows), float64(field.NonEmpty)/float64(field.Rows), *p.MaxFieldDrop) {
				add("field_drop", field.Name, field.NonEmpty)
			}
		}
	}
	return drift
}

// False with no error means the current observations cannot establish a schema.
// A failed transport window still deserves a report with its failure categories;
// an actual scope, policy or known schema mismatch remains an error.
func aggregateBaselineCompatible(report, baseline Report) (bool, error) {
	if validateReport(baseline) != nil {
		return false, ErrIncompatible
	}
	a, b := report.Query.Scope, baseline.Query.Scope
	if a.Site != b.Site || a.Stage != b.Stage || a.RuleID != b.RuleID || (!report.Policy.CompareAcrossVersions && a.Version != b.Version) || !reflect.DeepEqual(report.Policy, baseline.Policy) {
		return false, ErrIncompatible
	}
	if baseline.ID == "" || baseline.Coverage != "recorded_observations_only" || baseline.Truncated || baseline.Query.LostObservations != 0 || baseline.Verdict == Partial || baseline.Metrics.Pages < report.Policy.MinPages || baseline.Metrics.EvaluatedPages < report.Policy.MinPages || baseline.Metrics.DistinctRequests < report.Policy.MinRequests {
		return false, ErrIncompatible
	}
	// Relative policies need a first report without a baseline. Its verdict is
	// insufficient only because that comparison was unavailable; a complete,
	// adequately sampled report can still seed the host's explicit baseline.
	baselineRequired := false
	for _, signal := range baseline.Signals {
		switch signal.Code {
		case "baseline_required":
			baselineRequired = true
		case "sample_pages", "sample_requests", "sample_evaluated_pages", "sample_evaluated_requests", "incomplete_fields", "truncated_extraction", "mixed_field_schema", "truncated_observations", "lost_observations", "comparison_unavailable", "zero_rows":
			return false, ErrIncompatible
		}
	}
	if baseline.Verdict == Insufficient && (!baselineRequired || baseline.BaselineID != "" || baseline.Policy.MaxRowsDrop == nil && baseline.Policy.MaxFieldDrop == nil) {
		return false, ErrIncompatible
	}
	if baseline.FieldSchemaSHA256 == "" {
		return false, ErrIncompatible
	}
	if report.FieldSchemaSHA256 == "" {
		for _, signal := range report.Signals {
			if signal.Code == "mixed_field_schema" {
				return false, ErrIncompatible
			}
		}
		return false, nil
	}
	if baseline.FieldSchemaSHA256 != report.FieldSchemaSHA256 || len(report.Metrics.Fields) != len(baseline.Metrics.Fields) {
		return false, ErrIncompatible
	}
	fields := make(map[string]FieldStat, len(baseline.Metrics.Fields))
	for _, field := range baseline.Metrics.Fields {
		fields[field.Name] = field
	}
	for _, field := range report.Metrics.Fields {
		previous, exists := fields[field.Name]
		if !exists || previous.Kind != field.Kind || previous.Required != field.Required {
			return false, ErrIncompatible
		}
	}
	return true, nil
}

func aggregateEarlier(a, b Observation) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func aggregateFieldsFingerprint(fields []FieldStat) string {
	type fieldSchema struct {
		Name, Kind string
		Required   bool
	}
	value := make([]fieldSchema, len(fields))
	for i, field := range fields {
		value[i] = fieldSchema{field.Name, field.Kind, field.Required}
	}
	sort.Slice(value, func(i, j int) bool { return value[i].Name < value[j].Name })
	data, _ := json.Marshal(value)
	return string(data)
}
func aggregateScopeMatch(query, scope Scope) bool {
	return query == scope
}

func aggregateAdd(total *int, amount int) bool {
	if amount < 0 || *total > int(^uint(0)>>1)-amount {
		return false
	}
	*total += amount
	return true
}
func aggregateSignal(report *Report, code, field string, count int) {
	for i := range report.Signals {
		if report.Signals[i].Code == code && report.Signals[i].Field == field {
			report.Signals[i].Count += count
			return
		}
	}
	report.Signals = append(report.Signals, Signal{Code: code, Field: field, Count: count})
}
func aggregateRatioAtLeast(numerator, denominator int, threshold float64) bool {
	return numerator > 0 && denominator > 0 && float64(numerator)/float64(denominator) >= threshold
}
func aggregateDrop(before, after, threshold float64) bool {
	return before > 0 && after < before && (before-after)/before >= threshold
}
func aggregateBoundUint(value uint64) int {
	maximum := uint64(^uint(0) >> 1)
	if value > maximum {
		return int(maximum)
	}
	return int(value)
}
func aggregateClonePolicy(policy Policy) Policy {
	for _, target := range []**float64{&policy.ExtractionFailureRate, &policy.MissingRate, &policy.EmptyRate, &policy.MaxRowsDrop, &policy.MaxFieldDrop} {
		if *target != nil {
			value := **target
			*target = &value
		}
	}
	if policy.MinRows != nil {
		value := *policy.MinRows
		policy.MinRows = &value
	}
	return policy
}
