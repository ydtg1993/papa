package rulequality

import (
	"context"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

// ReconcileStopped uses no Agent/configuration and never makes model requests.
// The host must first establish that the old executor has ended everywhere.
func ReconcileStopped(ctx context.Context, store Store, audit agentstore.Store, id string, rev uint64, operator, source string) (ReanalysisOutcome, error) {
	if ctx == nil || collectorNil(store) || collectorNil(audit) || !validText(operator, 128) || !validText(source, 256) {
		return ReanalysisOutcome{}, ErrInvalid
	}
	r := &Reanalyzer{config: ReanalysisConfig{Store: store, Audit: audit, Actor: rulestore.Actor{Operator: operator, Source: source}}}
	return r.Reconcile(ctx, id, rev, true)
}

// Preview validates the host goal/current publication and selected references.
// It does not reserve a run or read/recreate a body; Execute rechecks retained
// snapshots and the current publication before any model call.
func (r *Reanalyzer) Preview(ctx context.Context, reportID string, input ReanalysisInput) (ReanalysisInput, string, error) {
	if ctx == nil {
		return input, "", ErrInvalid
	}
	report, err := r.config.Store.Report(ctx, reportID)
	if err != nil {
		return input, "", err
	}
	if report.Verdict != SuspectedDrift {
		return input, "", ErrNotActionable
	}
	normalized, digest, err := inputDigest(input)
	if err != nil {
		return input, "", err
	}
	if err = r.checkGoal(ctx, report, normalized.Goal); err != nil {
		return input, "", err
	}
	if len(normalized.EvidenceIDs) == 0 {
		return input, "", ErrEvidence
	}
	allowed := map[string]string{}
	for _, ref := range report.Evidence {
		if ref.EvidenceID != "" {
			if old, ok := allowed[ref.EvidenceID]; ok && old != ref.BodySHA256 {
				return input, "", ErrCorrupt
			}
			allowed[ref.EvidenceID] = ref.BodySHA256
		}
	}
	for _, id := range normalized.EvidenceIDs {
		if allowed[id] == "" {
			return input, "", ErrEvidence
		}
	}
	return normalized, digest, nil
}
