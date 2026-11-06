package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/extract"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

type ruleObservationKey struct{}
type ruleObservation struct {
	mu       sync.Mutex
	manifest ruleSnapshotManifest
	body     []byte // sanitized bytes only, shared by its single artifact registration
	policy   htmlfetch.SamplePolicy
	maxBytes int64
}

func (f *RuleFetcher) beginRuleSnapshot(ctx context.Context, task *Task, req RuleRequest, format string) context.Context {
	ar := archiveFromCtx(ctx)
	if ar == nil {
		return ctx
	}
	ar.rulesMu.Lock()
	defer ar.rulesMu.Unlock()
	if len(ar.rules) >= maxArchivePagesPerAttempt {
		task.Trace.Warn(ruleSnapshotTraceStep, nil, map[string]any{"state": "observation-limit"})
		return context.WithValue(ctx, ruleObservationKey{}, (*ruleObservation)(nil))
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		task.Trace.Warn(ruleSnapshotTraceStep, nil, map[string]any{"state": "id-unavailable"})
		return context.WithValue(ctx, ruleObservationKey{}, (*ruleObservation)(nil))
	}
	obs := &ruleObservation{maxBytes: ar.e.archiveMaxBytes(), manifest: ruleSnapshotManifest{
		Version: 1, EvidenceID: hex.EncodeToString(random[:]), Rule: RuleRef{task.Meta["rule_id"], task.Meta["rule_version"]},
		TaskID: task.ID, Site: task.Site, Stage: task.Stage, Attempt: ar.attempt, Retry: task.Retry, Repeat: task.Repeat,
		Format: format, Rendered: f.config.Transport == "browser", BodyOmitted: "response-unavailable", CreatedAt: time.Now().UTC(),
	}}
	if release, err := decodeRuleRelease(task.Meta[ruleReleaseMeta]); err == nil {
		obs.manifest.ReleaseID, obs.manifest.ReleaseGeneration = release.ID, release.Generation
	}
	if f.config.SnapshotPolicy != nil {
		obs.policy = *f.config.SnapshotPolicy
	}
	obs.manifest.SourceURL, _ = safeRuleSnapshotURL(req.URL, obs.policy)
	ar.rules = append(ar.rules, obs)
	return context.WithValue(ctx, ruleObservationKey{}, obs)
}

func ruleObservationFrom(ctx context.Context) *ruleObservation {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(ruleObservationKey{}).(*ruleObservation)
	return obs
}
func ruleSnapshotID(ctx context.Context) string {
	obs := ruleObservationFrom(ctx)
	if obs == nil {
		return ""
	}
	return obs.manifest.EvidenceID
}

func recordRuleSnapshot(ctx context.Context, rawURL string, status int, rendered bool, body []byte) {
	obs := ruleObservationFrom(ctx)
	if obs == nil {
		return
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	obs.manifest.StatusCode = status
	obs.manifest.Rendered = rendered
	obs.body = nil
	obs.manifest.BodyOmitted = "body-redaction-failed"
	obs.manifest.OriginalSHA256 = ""
	source, ok := safeRuleSnapshotURL(rawURL, obs.policy)
	obs.manifest.SourceURL = source
	if !ok {
		obs.manifest.BodyOmitted = "url-redaction-failed"
		return
	}
	if int64(len(body)) > obs.maxBytes {
		obs.manifest.BodyOmitted = "input-over-budget"
		return
	}
	obs.manifest.OriginalSHA256 = ruleDigest(body)
	if obs.policy.RedactBody == nil {
		obs.manifest.BodyOmitted = "body-policy-omitted"
		return
	}
	sanitized, ok := safeRuleSnapshotBody(obs.policy, body)
	if !ok {
		obs.manifest.BodyOmitted = "body-redaction-failed"
		return
	}
	if int64(len(sanitized)) > obs.maxBytes {
		obs.manifest.BodyOmitted = "redacted-over-budget"
		return
	}
	obs.body = bytes.Clone(sanitized)
	obs.manifest.BodyOmitted = ""
}

func recordRuleSnapshotUnavailable(ctx context.Context, rawURL string, status int, rendered bool, reason string) {
	obs := ruleObservationFrom(ctx)
	if obs == nil {
		return
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	obs.manifest.StatusCode = status
	obs.manifest.Rendered = rendered
	obs.manifest.BodyOmitted = reason
	obs.manifest.SourceURL, _ = safeRuleSnapshotURL(rawURL, obs.policy)
	obs.manifest.OriginalSHA256 = ""
	obs.body = nil
}

func recordRuleSnapshotValidation(ctx context.Context, report extract.Report) {
	obs := ruleObservationFrom(ctx)
	if obs == nil {
		return
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	// Reports contain codes/pointers, never field values. Cap observation details
	// separately from the rule's potentially much larger validation issue budget.
	if len(report.Issues) > 100 {
		report.Issues = report.Issues[:100]
		report.Truncated = true
	}
	report.Issues = slices.Clone(report.Issues)
	obs.manifest.Validation = &report
}

func safeRuleSnapshotURL(raw string, policy htmlfetch.SamplePolicy) (result string, ok bool) {
	defer func() {
		if recover() != nil {
			result = ""
			ok = false
		}
	}()
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}
	filtered := url.Values{}
	for _, key := range policy.QueryKeys {
		if values, found := query[key]; found {
			filtered[key] = values
		}
	}
	parsed.RawQuery = filtered.Encode()
	parsed.User = nil
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parsed.ForceQuery = false
	result = parsed.String()
	if policy.RedactURL != nil {
		result, err = policy.RedactURL(result)
		if err != nil {
			return "", false
		}
	}
	parsed, err = url.Parse(result)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || len(result) > 8192 {
		return "", false
	}
	return result, true
}

func safeRuleSnapshotBody(policy htmlfetch.SamplePolicy, raw []byte) (body []byte, ok bool) {
	defer func() {
		if recover() != nil {
			body = nil
			ok = false
		}
	}()
	body, err := policy.RedactBody(false, bytes.Clone(raw))
	return body, err == nil
}

func (a *archiveBuffer) finishRuleSnapshots(failed bool) {
	a.rulesMu.Lock()
	observations := a.rules
	a.rules = nil
	a.rulesMu.Unlock()
	for _, obs := range observations {
		obs.mu.Lock()
		manifest := obs.manifest
		body := obs.body
		obs.body = nil
		obs.mu.Unlock()
		data := map[string]any{"evidence_id": manifest.EvidenceID, "rule_id": manifest.Rule.ID, "rule_version": manifest.Rule.Version}
		if manifest.ReleaseID != "" {
			data["release_id"], data["release_generation"] = manifest.ReleaseID, manifest.ReleaseGeneration
		}
		if !failed && a.e.archiveMode() != "always" {
			data["state"] = "not-retained"
			a.task.Trace.ruleSnapshotReference(data, false)
			continue
		}
		manifest.Failed = failed
		stored, file, err := writeRuleSnapshot(a.e.archiveDir(), manifest, body)
		if err != nil {
			data["state"] = "write-failed"
			a.e.loggerSet.Engine.Warn("rule snapshot: publication failed (task result unchanged)")
			a.task.Trace.ruleSnapshotReference(data, true)
			continue
		}
		files := []string{file}
		data["state"] = "stored"
		if stored.BodyFile != "" {
			files = append(files, stored.BodyFile)
			data["body_sha256"] = stored.BodySHA256
		} else {
			data["state"] = "metadata-only"
			data["body_omitted"] = stored.BodyOmitted
		}
		data["files"] = files
		a.task.Trace.ruleSnapshotReference(data, false)
	}
}

// Keep only the small, framework-constructed reference on successful attempts.
// Ordinary Step data still follows the existing failure-only retention policy.
func (t *Trace) ruleSnapshotReference(data map[string]any, warning bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	if t.referenceSeq == nil {
		t.referenceSeq = map[int]bool{}
	}
	t.referenceSeq[len(t.steps)] = true
	status := models.TraceOK
	if warning {
		status = models.TraceWarn
	}
	t.append(ruleSnapshotTraceStep, status, "", "", data)
}
