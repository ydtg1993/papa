package breakerstore

import (
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

func snapshotFixture(l incident.Lifecycle) Snapshot {
	s := Snapshot{Lifecycle: l, Window: &breaker.Checkpoint{}, Control: breaker.Recovery{Version: 1, ID: l.Capture.ID, Site: l.Capture.Site, Stage: l.Stage, State: l.State, Revision: l.Revision, Epoch: l.Epoch, StartedAt: l.StartedAt, UpdatedAt: l.UpdatedAt, LastFailureAt: l.LastFailureAt, NextProbeAt: l.NextProbeAt, RetryAfterUntil: l.RetryAfterUntil, Deadline: l.Deadline, FinalAt: l.FinalAt, CountersResetAt: l.CountersResetAt, ProbeAttempts: l.ProbeAttempts, Successes: l.Successes, ChecksLimited: l.ChecksLimited}}
	for _, c := range l.Checks {
		s.Control.Checks = append(s.Control.Checks, breaker.StageCheck{Stage: c.Stage, State: c.State, Round: c.Round, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt, Detail: c.Detail, TaskSuccesses: c.TaskSuccesses, Verification: c.Verification, Coverage: c.Coverage, ProbeSuccesses: c.ProbeSuccesses})
	}
	return s
}
