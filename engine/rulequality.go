package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/ydtg1993/papa/v3/pkg/account"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

// Attempt identity must remain available when both Trace and archive are off.
// Task.Retry counts queue reprocessing; attempt is the local handler retry.
type ruleAttemptKey struct{}
type ruleQualityKey struct{}

type ruleQualityRun struct {
	collector *rulequality.Collector
	trace     *Trace
	value     rulequality.Observation
}

func withRuleAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, ruleAttemptKey{}, attempt)
}

func (f *RuleFetcher) beginRuleQuality(ctx context.Context, task *Task, ref RuleRef, requestHash string, page int) (context.Context, *ruleQualityRun) {
	if f.config.Quality == nil {
		// Host callbacks may invoke another fetcher with the same context.
		// Its disabled collector must not overwrite the outer run's summary.
		if ruleQualityFrom(ctx) != nil {
			return context.WithValue(ctx, ruleQualityKey{}, (*ruleQualityRun)(nil)), nil
		}
		return ctx, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		task.Trace.Warn("规则质量观测", nil, map[string]any{"state": "id-unavailable"})
		return ctx, nil
	}
	release, _ := decodeRuleRelease(task.Meta[ruleReleaseMeta]) // taskState already validated it
	attempt, _ := ctx.Value(ruleAttemptKey{}).(int)
	q := &ruleQualityRun{collector: f.config.Quality, trace: task.Trace, value: rulequality.Observation{
		ID: hex.EncodeToString(random[:]),
		Scope: rulequality.Scope{Site: task.Site, Stage: task.Stage, RuleID: ref.ID, Version: ref.Version,
			ReleaseID: release.ID, ReleaseGeneration: release.Generation},
		TaskID: task.ID, Repeat: task.Repeat, Reprocess: task.Retry, Attempt: attempt,
		RequestSHA256: requestHash, Page: page, Operation: "rule", FailureKind: rulequality.FailureRule,
		Rendered: f.config.Transport == "browser",
	}}
	if task.ID != 0 {
		// Reprocess and attempt are deliberately absent: retries of the same
		// logical page must not inflate the report's independent-page count.
		q.value.LogicalKey = ruleDigest([]byte(ruleJSON(struct {
			TaskID, Repeat int
			Request        string
			Scope          rulequality.Scope
		}{task.ID, task.Repeat, requestHash, q.value.Scope})))
	} else {
		// With no host key, reports fold by this observation's unique ID.
		q.value.LogicalKey = rulequality.LogicalKey(ctx)
	}
	// A callback may start another fetcher with its parent's context. This run
	// does not own the parent's snapshot; early stops and rule-load failures
	// happen before beginRuleSnapshot can allocate a fresh identity. Keep the
	// shared archive buffer, but mask that inherited identity in the child only.
	ctx = context.WithValue(ctx, ruleObservationKey{}, (*ruleObservation)(nil))
	return context.WithValue(ctx, ruleQualityKey{}, q), q
}

func ruleQualityFrom(ctx context.Context) *ruleQualityRun {
	q, _ := ctx.Value(ruleQualityKey{}).(*ruleQualityRun)
	return q
}

func (q *ruleQualityRun) phase(operation, failure string) {
	if q != nil {
		q.value.Operation, q.value.FailureKind = operation, failure
	}
}

func (q *ruleQualityRun) stop(reason string) {
	if q != nil {
		q.value.StopReason = reason
	}
}

func (q *ruleQualityRun) response(status int, rendered bool, body []byte) {
	if q == nil {
		return
	}
	q.value.HTTPStatus, q.value.Rendered = status, rendered
	q.value.CompleteResponse = body != nil
	q.value.BodySHA256 = ""
	if body != nil {
		q.value.BodySHA256 = ruleDigest(body)
	}
}

func (q *ruleQualityRun) evaluated(rule crawlrule.Rule, result crawlrule.Result, err error) {
	if q == nil {
		return
	}
	q.value.Evaluated = true
	q.value.ExtractionOK = err == nil && !result.Truncated && !result.Validation.Truncated
	q.value.RawRows, q.value.InvalidRows = len(result.Records), result.Validation.InvalidRows
	q.value.Truncated = result.Truncated || result.Validation.Truncated
	q.value.Fields, q.value.FieldsIncomplete = rulequality.Summarize(rule, result)
	q.value.FieldSchemaSHA256 = rulequality.FieldSchemaDigest(rule)
	q.value.HasNext = result.NextURL != "" || result.NextCursor != ""
	switch {
	case errors.Is(err, crawlrule.ErrBodyTooLarge), q.value.Truncated:
		q.value.FailureKind = rulequality.FailureBudget
	case errors.Is(err, crawlrule.ErrValidation):
		q.value.FailureKind = rulequality.FailureValidation
	case errors.Is(err, crawlrule.ErrNextURL), errors.Is(err, crawlrule.ErrNextCursor):
		q.value.FailureKind = rulequality.FailurePlanning
	case errors.Is(err, crawlrule.ErrNoRecords) && len(result.Records) == 0:
		// Empty extraction is observable but alone is not evidence of drift.
		q.value.StopReason = "no_records"
	}
}

// Finish never passes storage failures back to the business handler. Observe
// owns timeout/panic isolation; warnings contain stable codes, never its error.
func (q *ruleQualityRun) finish(ctx context.Context, err error, panicked bool) {
	if q == nil {
		return
	}
	q.value.EvidenceID = ruleSnapshotID(ctx)
	switch {
	case panicked:
		q.value.Operation, q.value.FailureKind = "panic", rulequality.FailureHost
	case errors.Is(err, context.Canceled), ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		q.value.FailureKind = rulequality.FailureCanceled
	case err == nil:
		q.value.Operation, q.value.FailureKind = "complete", ""
	}
	if q.collector.Observe(context.WithoutCancel(ctx), q.value) != nil {
		q.trace.Warn("规则质量观测", nil, map[string]any{"state": "record-failed"})
	}
}

// This classifier uses explicit account identities and wire status only. The
// source branch supplies network/render defaults; raw error text is never read.
func ruleQualitySourceFailure(ctx context.Context, err error, status int, fallback string) {
	q := ruleQualityFrom(ctx)
	if q == nil {
		return
	}
	kind := fallback
	switch {
	case errors.Is(err, account.ErrChallenge):
		kind = rulequality.FailureChallenge
	case errors.Is(err, account.ErrUnknown), errors.Is(err, account.ErrPaused), errors.Is(err, account.ErrExpired),
		errors.Is(err, account.ErrForbidden), errors.Is(err, account.ErrSuperseded), errors.Is(err, account.ErrUnsupported):
		kind = rulequality.FailureAuthentication
	case status == http.StatusTooManyRequests:
		kind = rulequality.FailureRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = rulequality.FailureAuthentication
	case status != 0 && (status < 200 || status >= 300):
		kind = rulequality.FailureHTTP
	}
	q.value.FailureKind = kind
}
