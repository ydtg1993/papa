package breakerstore

import (
	"fmt"
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

// ValidateCurrent rejects snapshots that cannot supply v4 recovery evidence.
func (s Snapshot) ValidateCurrent() error {
	if s.Control.Version != 1 || s.Control.ID == "" || s.Window == nil {
		return fmt.Errorf("unsupported breaker snapshot: v4 control version 1 and exact window required")
	}
	return nil
}
