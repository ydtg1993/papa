package breakerstore

import (
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

// Snapshot atomically pairs operational recovery data with its display report.
// The existing JSON column stores this envelope; History is appended separately
// in the same transaction, never used to drive admission.
type Snapshot struct {
	incident.Lifecycle `json:"report"`
	Control            breaker.Recovery      `json:"control"`
	Window             *breaker.Checkpoint   `json:"window"`
	History            []incident.Transition `json:"-"` // unacknowledged append-only records, saved atomically with control
}

func (s Snapshot) Clean() Snapshot {
	if s.Control.PendingFailure != nil {
		p := *s.Control.PendingFailure
		s.Control.PendingFailure = &p
	}
	s.Lifecycle = s.Lifecycle.Clean()
	s.Control.Checks = append([]breaker.StageCheck(nil), s.Control.Checks...)
	for i := range s.Control.Checks {
		s.Control.Checks[i].Samples = append([]breaker.RecoverySample(nil), s.Control.Checks[i].Samples...)
		s.Control.Checks[i].SuccessIDs = append([]string(nil), s.Control.Checks[i].SuccessIDs...)
		s.Control.Checks[i].TimedOutIDs = append([]string(nil), s.Control.Checks[i].TimedOutIDs...)
		s.Control.Checks[i].Detail = incident.Clean(incident.Sample{Error: s.Control.Checks[i].Detail}).Error
		s.Control.Checks[i].Coverage = incident.Clean(incident.Sample{Error: s.Control.Checks[i].Coverage}).Error
	}
	// Clean each record separately: display's 128-entry bound must not trim unsaved history.
	s.History = append([]incident.Transition(nil), s.History...)
	for i, entry := range s.History {
		s.History[i] = (incident.Lifecycle{Timeline: []incident.Transition{entry}}).Clean().Timeline[0]
	}
	if s.Window != nil {
		w := *s.Window
		w.Failures = append([]breaker.Outcome(nil), w.Failures...)
		s.Window = &w
	}
	return s
}

// UpgradeRecovery is a one-time compatibility conversion, not runtime control
// driven by the display report. Old promoted hard incidents may have started mixed.
func (s *Snapshot) UpgradeRecovery() {
	if s.Control.Version != 0 {
		return
	}
	if s.Control.Active() && (s.Rule == "mixed" || s.Rule == "hard" || s.Rule == "" || s.Control.State == "observing") {
		s.Control.MixedSamples = true
	}
	s.Control.Version = 1
}

// LegacySnapshot is only for upgrading old stored events. Missing exact window
// evidence is explicit and forces manual protection in Engine.
func LegacySnapshot(l incident.Lifecycle) Snapshot {
	s := Snapshot{Lifecycle: l, Control: breaker.Recovery{ID: l.Capture.ID, Site: l.Capture.Site, Stage: l.Stage, State: l.State, Revision: l.Revision, Epoch: l.Epoch, StartedAt: l.StartedAt, UpdatedAt: l.UpdatedAt, LastFailureAt: l.LastFailureAt, NextProbeAt: l.NextProbeAt, RetryAfterUntil: l.RetryAfterUntil, Deadline: l.Deadline, FinalAt: l.FinalAt, CountersResetAt: l.CountersResetAt, ProbeAttempts: l.ProbeAttempts, Successes: l.Successes, ChecksLimited: l.ChecksLimited}}
	for _, c := range l.Checks {
		s.Control.Checks = append(s.Control.Checks, breaker.StageCheck{Stage: c.Stage, State: c.State, Round: c.Round, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt, Detail: c.Detail, TaskSuccesses: c.TaskSuccesses, Verification: c.Verification, Coverage: c.Coverage, ProbeSuccesses: c.ProbeSuccesses})
	}
	return s
}
