package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCompensationIsSeparateFromImmediateRetry(t *testing.T) {
	cause := errors.New("synthetic failure")
	if _, ok := CompensationOf(WrapNoRetry(cause)); ok {
		t.Fatal("NoRetry unexpectedly declared permanent failure")
	}
	deadline := time.Now().Add(time.Minute)
	for _, err := range []error{RetryAfter(cause, deadline), ManualCompensation(cause, "operator required"), PermanentFailure(cause, "record removed")} {
		wrapped := fmt.Errorf("stage: %w", err)
		if Retryable(wrapped) || !errors.Is(wrapped, cause) {
			t.Fatalf("policy lost cause or allowed immediate retry: %v", wrapped)
		}
		if _, ok := CompensationOf(wrapped); !ok {
			t.Fatal("wrapped policy lost")
		}
	}
	policy, _ := CompensationOf(RetryAfter(cause, deadline))
	if !policy.NotBefore.Equal(deadline) {
		t.Fatal("deadline lost")
	}
	if WithCompensation(nil, CompensationPolicy{}) != nil {
		t.Fatal("nil cause became error")
	}
}

func TestInvalidCompensationFailsClosed(t *testing.T) {
	for _, policy := range []CompensationPolicy{{Mode: "typo"}, {Mode: CompensationRetryAfter}, {Mode: CompensationRetryAfter, NotBefore: time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)}} {
		got, _ := CompensationOf(WithCompensation(errors.New("x"), policy))
		if got.Mode != CompensationManual {
			t.Fatalf("invalid policy allowed retry: %+v", got)
		}
	}
}
