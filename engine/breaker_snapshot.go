package engine

import (
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

// Control is authoritative. The report is only a bounded history plus a
// projection of current control, never input to admission or stage matching.
type breakerEvent struct {
	breaker.Recovery
	report         incident.Lifecycle
	pendingHistory []incident.Transition
}

func (l *breakerEvent) reportSnapshot() incident.Lifecycle {
	r := l.report
	r.Capture.ID, r.Capture.Site = l.ID, l.Site
	r.Stage, r.State, r.Revision, r.Epoch = l.Stage, l.State, l.Revision, l.Epoch
	r.StartedAt, r.UpdatedAt, r.LastFailureAt = l.StartedAt, l.UpdatedAt, l.LastFailureAt
	r.NextProbeAt, r.Deadline, r.FinalAt = l.NextProbeAt, l.Deadline, l.FinalAt
	r.CountersResetAt = l.CountersResetAt
	r.RetryAfterUntil = l.RetryAfterUntil
	r.RecheckAt, r.Cycle = l.RecheckAt, l.Cycle
	r.ProbeAttempts, r.Successes, r.ChecksLimited = l.ProbeAttempts, l.Successes, l.ChecksLimited
	r.Checks = make([]incident.StageCheck, len(l.Checks))
	for i, c := range l.Checks {
		verified := c.TaskSuccesses >= max(1, c.RequiredTasks) && c.State != "failed" && c.State != "missing" && c.State != "interrupted"
		if c.Verification == "readonly" {
			verified = c.State == "passed" && c.ProbeSuccesses >= max(2, c.RequiredProbes)
		}
		r.Checks[i] = incident.StageCheck{Stage: c.Stage, State: c.State, Round: c.Round, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt, Detail: c.Detail, TaskSuccesses: c.TaskSuccesses, Verification: c.Verification, Coverage: c.Coverage, ProbeSuccesses: c.ProbeSuccesses, RequiredTasks: c.RequiredTasks, RequiredProbes: c.RequiredProbes, Verified: verified}
		for _, sample := range c.Samples {
			r.Checks[i].SampleIDs = append(r.Checks[i].SampleIDs, recoverySampleID(sample))
		}
	}
	return r.Clean()
}

func (s *smartBreaker) snapshot() breakerstore.Snapshot {
	w := s.detector.Checkpoint()
	return (breakerstore.Snapshot{Lifecycle: s.event.reportSnapshot(), Control: s.event.Recovery, Window: &w, History: s.event.pendingHistory}).Clean()
}

// Only report storage is cleaned here; no cleaned data flows back into control.
func (l *breakerEvent) trimReport() { l.report = l.report.Clean() }
