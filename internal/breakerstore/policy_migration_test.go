package breakerstore

import (
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"testing"
	"time"
)

func TestPolicyMigrationPreservesLegacyRecoveryBudget(t *testing.T) {
	s := Snapshot{Control: breaker.Recovery{Version: 1, ID: "legacy", Site: "fixture", State: "manual", ProbeAttempts: 2, Cycle: 7, Deadline: time.Now().Add(time.Minute)}, Window: &breaker.Checkpoint{Failures: []breaker.Outcome{{TaskID: 42}}}}
	if err := s.ValidateCurrent(); err == nil {
		t.Fatal("legacy silently accepted")
	}
	c := breaker.EffectiveConfig(breaker.Config{Enabled: true, Threshold: 10})
	if _, err := MigratePolicySnapshot(s, 0, c); err == nil {
		t.Fatal("unknown source revision accepted")
	}
	got, err := MigratePolicySnapshot(s, 5, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = got.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	if got.Control.ID != s.Control.ID || got.Control.Deadline != s.Control.Deadline || got.Control.ProbeAttempts != 2 || got.Control.Cycle != 7 || got.Window.Failures[0].TaskID != 42 {
		t.Fatal("migration reset state")
	}
	if s.Control.Version != 1 {
		t.Fatal("migration mutated source")
	}
}
