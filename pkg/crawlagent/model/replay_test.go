package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestReplaySequenceAndDetachedSnapshots(t *testing.T) {
	steps := []ReplayStep{{Reply: Reply{Content: "first"}}, {Err: ErrRateLimit}, {Reply: Reply{Content: "third"}}}
	replay := NewReplay(steps...)
	steps[0].Reply.Content = "mutated"
	messages := []Message{{Role: "user", Content: "original"}}
	for i, want := range []string{"first", "", "third", ""} {
		reply, err := replay.Complete(context.Background(), messages, 12)
		if reply.Content != want || (i == 1 && !errors.Is(err, ErrRateLimit)) || (i == 3 && !errors.Is(err, ErrExhausted)) || ((i == 0 || i == 2) && err != nil) {
			t.Fatalf("step=%d reply=%+v error=%v", i, reply, err)
		}
	}
	messages[0].Content = "caller mutation"
	calls := replay.Calls()
	if len(calls) != 4 || calls[0].Messages[0].Content != "original" || calls[0].MaxOutputTokens != 12 {
		t.Fatalf("calls=%+v", calls)
	}
	calls[0].Messages[0].Content = "snapshot mutation"
	calls[0].MaxOutputTokens = 999
	if replay.Calls()[0].Messages[0].Content != "original" || replay.Calls()[0].MaxOutputTokens != 12 {
		t.Fatal("returned snapshot aliases stored calls")
	}
}

func TestReplayCanceledAndInvalidInputsDoNotConsumeSteps(t *testing.T) {
	replay := NewReplay(ReplayStep{Reply: Reply{Content: "available"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := replay.Complete(ctx, testMessages(), 12); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := replay.Complete(context.Background(), nil, 12); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err := replay.Complete(context.Background(), testMessages(), 0); !errors.Is(err, ErrOutputLimit) {
		t.Fatal(err)
	}
	if len(replay.Calls()) != 0 {
		t.Fatal("invalid call consumed a step")
	}
	if reply, err := replay.Complete(context.Background(), testMessages(), 12); err != nil || reply.Content != "available" {
		t.Fatalf("reply=%+v error=%v", reply, err)
	}
}

func TestReplayConcurrentCallsConsumeEachDecisionOnce(t *testing.T) {
	const count = 32
	steps := make([]ReplayStep, count)
	for i := range steps {
		steps[i].Reply.Content = fmt.Sprint(i)
	}
	replay := NewReplay(steps...)
	results := make(chan string, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			reply, err := replay.Complete(context.Background(), testMessages(), 12)
			if err != nil {
				t.Error(err)
			}
			results <- reply.Content
			_ = replay.Calls()
		}()
	}
	group.Wait()
	close(results)
	seen := make(map[string]bool)
	for result := range results {
		if seen[result] {
			t.Fatalf("decision repeated: %q", result)
		}
		seen[result] = true
	}
	if len(seen) != count || len(replay.Calls()) != count {
		t.Fatal("decisions lost")
	}
}
