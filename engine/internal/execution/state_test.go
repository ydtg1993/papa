package execution

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"testing"
)

type quietLog struct{}

func (quietLog) Errorf(string, ...any) {}
func TestStateRetryCancellationRetainsUnconfirmedOnly(t *testing.T) {
	var s State
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	failure := errors.New("storage unavailable")
	err := s.Retry(ctx, 7, "site:stage", "complete", func() error { calls++; cancel(); return failure }, quietLog{})
	if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, failure) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	snap := s.Snapshot()
	if snap[7] != "site:stage" {
		t.Fatal(snap)
	}
	delete(snap, 7)
	if len(s.Snapshot()) != 1 {
		t.Fatal("snapshot aliases state")
	}
	s.Forget(7)
	if len(s.Snapshot()) != 0 {
		t.Fatal("confirmed work remains unresolved")
	}
}
func TestStateRetryGenerationConflictDoesNotRetry(t *testing.T) {
	var s State
	calls := 0
	err := s.Retry(context.Background(), 7, "s", "complete", func() error { calls++; return taskstore.ErrChanged }, quietLog{})
	if !errors.Is(err, taskstore.ErrChanged) || calls != 1 || len(s.Snapshot()) != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
