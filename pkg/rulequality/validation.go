package rulequality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func validText(s string, max int) bool {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func validCode(s string, max int) bool {
	if !validText(s, max) {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
func validSHA(s string, optional bool) bool {
	if s == "" {
		return optional
	}
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validRunID(s string) bool {
	if !validCode(s, 64) {
		return false
	}
	return !strings.ContainsAny(s, ".:")
}
func validateScope(s Scope) error {
	for _, v := range []string{s.Site, s.Stage, s.RuleID, s.Version} {
		if !validText(v, 128) {
			return ErrInvalid
		}
	}
	if s.ReleaseID != "" && !validText(s.ReleaseID, 128) {
		return ErrInvalid
	}
	if s.ReleaseID == "" && s.ReleaseGeneration != 0 {
		return ErrInvalid
	}
	return nil
}
func normalizeQuery(q Query) (Query, error) {
	if err := validateScope(q.Scope); err != nil {
		return Query{}, err
	}
	if q.From.IsZero() || q.Until.IsZero() || !q.From.Before(q.Until) || q.Limit < 0 || q.Limit > MaxObservations {
		return Query{}, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = MaxObservations
	}
	q.From = q.From.UTC()
	q.Until = q.Until.UTC()
	return q, nil
}
func validatePolicy(p Policy) error {
	if p.MinPages <= 0 || p.MinPages > MaxObservations || p.MinRequests <= 0 || p.MinRequests > MaxObservations || p.MinRows != nil && *p.MinRows < 0 {
		return ErrInvalid
	}
	for _, v := range []*float64{p.ExtractionFailureRate, p.MissingRate, p.EmptyRate, p.MaxRowsDrop, p.MaxFieldDrop} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 1) {
			return ErrInvalid
		}
	}
	return nil
}
func validFailure(s string) bool {
	switch s {
	case "", FailureNetwork, FailureHTTP, FailureRateLimit, FailureAuthentication, FailureChallenge, FailureRender, FailureRule, FailureExtraction, FailureValidation, FailureBudget, FailureSave, FailureSubmit, FailurePlanning, FailureCanceled, FailureHost:
		return true
	}
	return false
}
func validateFields(fs []FieldStat, rows int) error {
	if len(fs) > MaxFields {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !validText(f.Name, 128) || !validCode(f.Kind, 32) || seen[f.Name] {
			return ErrInvalid
		}
		seen[f.Name] = true
		if f.Rows < 0 || f.Rows > rows || f.Present < 0 || f.Present > f.Rows || f.Null < 0 || f.Null > f.Present || f.NonEmpty < 0 || f.NonEmpty > f.Present-f.Null || f.Invalid < 0 || f.Invalid > f.Rows {
			return ErrInvalid
		}
	}
	return nil
}
func validateObservation(o Observation) error {
	if !validCode(o.ID, 128) || validateScope(o.Scope) != nil || !validSHA(o.RequestSHA256, false) || !validSHA(o.LogicalKey, true) || !validSHA(o.FieldSchemaSHA256, true) || !validSHA(o.BodySHA256, true) || o.EvidenceID != "" && !validCode(o.EvidenceID, 128) {
		return ErrInvalid
	}
	if o.TaskID < 0 || o.Repeat < 0 || o.Reprocess < 0 || o.Attempt < 0 || o.RawRows < 0 || o.NewRows < 0 || o.NewRows > o.RawRows || o.InvalidRows < 0 || o.InvalidRows > o.RawRows || o.Page < 0 || o.PlannedChildren < 0 || o.SubmittedChildren < 0 || o.SubmittedChildren > o.PlannedChildren {
		return ErrInvalid
	}
	if o.HTTPStatus != 0 && (o.HTTPStatus < 100 || o.HTTPStatus > 599) || !validFailure(o.FailureKind) || !validCode(o.Operation, 32) || o.StopReason != "" && !validCode(o.StopReason, 64) {
		return ErrInvalid
	}
	if o.ExtractionOK && !o.Evaluated {
		return ErrInvalid
	}
	return validateFields(o.Fields, o.RawRows)
}
func validateReport(r Report) error {
	q, err := normalizeQuery(r.Query)
	if err != nil || q.Limit != r.Query.Limit || !validCode(r.ID, 128) || !validSHA(r.InputSHA256, false) || !validSHA(r.FieldSchemaSHA256, true) || validatePolicy(r.Policy) != nil || r.Coverage != "recorded_observations_only" || !validText(r.Folding, 256) {
		return ErrInvalid
	}
	if r.BaselineID != "" && !validCode(r.BaselineID, 128) || len(r.Evidence) > MaxReferences || len(r.Signals) > MaxFields*8 {
		return ErrInvalid
	}
	switch r.Verdict {
	case Insufficient, Partial, ObservedOK, SuspectedDrift, UpstreamFailure, ProcessingFailure:
	default:
		return ErrInvalid
	}
	m := r.Metrics
	if m.Attempts < 0 || m.Attempts > q.Limit || m.Pages < 0 || m.Pages > m.Attempts || m.DistinctRequests < 0 || m.DistinctRequests > m.Pages || m.EvaluatedPages < 0 || m.EvaluatedPages > m.Pages || m.ExtractionFailures < 0 || m.ExtractionFailures > m.Pages || m.RawRows < 0 || m.NewRows < 0 || m.NewRows > m.RawRows || m.InvalidRows < 0 || m.InvalidRows > m.RawRows {
		return ErrInvalid
	}
	if validateFields(m.Fields, m.RawRows) != nil || len(m.FailureCounts) > 32 {
		return ErrInvalid
	}
	for k, v := range m.FailureCounts {
		if k == "" || !validFailure(k) || v < 0 || v > m.Attempts {
			return ErrInvalid
		}
	}
	for _, v := range r.Signals {
		if !validCode(v.Code, 64) || v.Field != "" && !validText(v.Field, 128) || v.Count < 0 {
			return ErrInvalid
		}
	}
	for _, v := range r.Evidence {
		if !validCode(v.ObservationID, 128) || v.EvidenceID != "" && !validCode(v.EvidenceID, 128) || !validSHA(v.BodySHA256, true) {
			return ErrInvalid
		}
	}
	return nil
}
func encodePayload(v any) (string, string, error) {
	b, err := json.Marshal(v)
	if err != nil || len(b) > MaxPayloadBytes {
		return "", "", ErrInvalid
	}
	return string(b), payloadDigest(b), nil
}
func payloadDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func scopeDigest(s Scope) string    { b, _ := json.Marshal(s); return payloadDigest(b) }
func decodePayload(raw, hash string, v any) error {
	if len(raw) > MaxPayloadBytes || payloadDigest([]byte(raw)) != hash {
		return ErrCorrupt
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return ErrCorrupt
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) != raw {
		return ErrCorrupt
	}
	return nil
}
func detached[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func checkedKey(ctx context.Context, id string) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validCode(id, 128) {
		return ErrInvalid
	}
	return nil
}
func validResult(s string) bool {
	if len(s) > MaxResultBytes {
		return false
	}
	if s == "" {
		return true
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &obj) == nil && obj != nil
}
func prepareRequest(r Request) (Request, error) {
	if !validCode(r.ID, 128) || !validCode(r.OperationID, 128) || !validCode(r.ReportID, 128) || !validRunID(r.RunID) || !validText(r.Operator, 128) || !validText(r.Source, 256) || r.Status != "" && r.Status != Pending || r.Revision != 0 || !r.CreatedAt.IsZero() || !r.UpdatedAt.IsZero() || r.InputSHA256 != "" || r.ResultJSON != "" {
		return Request{}, ErrInvalid
	}
	r.Status = Pending
	r.Revision = 1
	return r, nil
}
func validateStoredRequest(r Request) error {
	if !validCode(r.ID, 128) || !validCode(r.OperationID, 128) || !validCode(r.ReportID, 128) || !validRunID(r.RunID) || !validText(r.Operator, 128) || !validText(r.Source, 256) || r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || !validResult(r.ResultJSON) {
		return ErrCorrupt
	}
	switch r.Status {
	case Pending:
		if r.Revision != 1 || r.InputSHA256 != "" || r.ResultJSON != "" {
			return ErrCorrupt
		}
	case Running:
		if r.Revision != 2 || !validSHA(r.InputSHA256, false) || r.ResultJSON != "" {
			return ErrCorrupt
		}
	case Dismissed:
		if r.Revision != 2 || r.InputSHA256 != "" {
			return ErrCorrupt
		}
	case AwaitingReview, NeedsEvidence, NeedsFixtures, Failed, Canceled, Interrupted:
		if r.Revision != 3 || !validSHA(r.InputSHA256, false) {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}
func requestEquivalent(a, b Request) bool {
	return a.OperationID == b.OperationID && a.ReportID == b.ReportID && a.Operator == b.Operator && a.Source == b.Source
}
func changeRequest(r Request, rev uint64, start bool, status, input string, now time.Time) (Request, error) {
	if rev == 0 || r.Revision != rev {
		return Request{}, ErrConflict
	}
	if start {
		if r.Status != Pending {
			return Request{}, ErrConflict
		}
		if !validSHA(input, false) {
			return Request{}, ErrInvalid
		}
		r.Status = Running
		r.InputSHA256 = input
	} else {
		if !validResult(input) {
			return Request{}, ErrInvalid
		}
		if status == Dismissed {
			if r.Status != Pending {
				return Request{}, ErrConflict
			}
		} else {
			switch status {
			case AwaitingReview, NeedsEvidence, NeedsFixtures, Failed, Canceled, Interrupted:
			default:
				return Request{}, ErrInvalid
			}
			if r.Status != Running {
				return Request{}, ErrConflict
			}
		}
		r.Status = status
		r.ResultJSON = input
	}
	r.Revision++
	r.UpdatedAt = now
	return r, nil
}
func checkedPrune(ctx context.Context, before time.Time, limit int) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if before.IsZero() || limit <= 0 || limit > MaxObservations {
		return fmt.Errorf("%w: prune requires cutoff and limit 1..%d", ErrInvalid, MaxObservations)
	}
	return nil
}
