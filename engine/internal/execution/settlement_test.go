package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/taskstore"
)

func TestConfirmSuccessCancellationRace(t *testing.T) {
	type versionKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), versionKey{}, 17))
	defer cancel()
	calls := 0
	err := ConfirmSuccess(ctx, func(c context.Context) error {
		calls++
		if calls == 1 {
			cancel()
			return context.Canceled
		}
		if c.Err() != nil || c.Value(versionKey{}) != 17 {
			t.Fatal("lost live context or execution version")
		}
		deadline, ok := c.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
			t.Fatal("settlement must be bounded")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestConfirmSuccessDoesNotReplayConfirmedOrStaleWrite(t *testing.T) {
	for _, result := range []error{nil, taskstore.ErrChanged} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := ConfirmSuccess(ctx, func(context.Context) error { calls++; cancel(); return result })
		cancel()
		if !errors.Is(err, result) || calls != 1 {
			t.Fatalf("result=%v err=%v calls=%d", result, err, calls)
		}
	}
}

func TestConfirmSuccessFailedFinalWriteRemainsUnconfirmed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sentinel := errors.New("unavailable")
	s := &State{}
	calls := 0
	err := ConfirmSuccess(ctx, func(c context.Context) error {
		calls++
		if c.Err() != nil {
			t.Fatal("no cancellation grace")
		}
		short, stop := context.WithTimeout(c, 20*time.Millisecond)
		defer stop()
		return s.Retry(short, 1, "alpha:detail", "terminal", func() error { return sentinel }, quietLog{})
	})
	if !errors.Is(err, sentinel) || calls != 1 || len(s.Snapshot()) != 1 {
		t.Fatalf("err=%v calls=%d pending=%v", err, calls, s.Snapshot())
	}
}
