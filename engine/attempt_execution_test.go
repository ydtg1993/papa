package engine

import (
	"context"
	"errors"
	"testing"
)

func TestAttemptExecutorInvokesBusinessOnceAndPreservesError(t *testing.T) {
	task := &Task{}
	session := attemptExecution{task: task}
	calls := 0
	failure := errors.New("business failure")
	err := session.run(context.Background(), func(context.Context) error { calls++; return failure })
	if calls != 1 || !errors.Is(err, failure) {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}
func TestAttemptExecutorRepanicsOriginalValue(t *testing.T) {
	session := attemptExecution{task: &Task{}}
	token := &struct{}{}
	calls := 0
	defer func() {
		if got := recover(); got != token {
			t.Fatalf("panic=%v", got)
		}
		if calls != 1 {
			t.Fatalf("calls=%d", calls)
		}
	}()
	_ = session.run(context.Background(), func(context.Context) error { calls++; panic(token) })
	t.Fatal("panic swallowed")
}
