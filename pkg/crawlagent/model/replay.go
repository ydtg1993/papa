package model

import (
	"context"
	"sync"
)

// ReplayStep is a host-authored deterministic reply or error, never a real model
// quality measurement. Error values should themselves be safe to expose.
type ReplayStep struct {
	Reply Reply
	Err   error
}

// Call is a detached snapshot of a replay input.
type Call struct {
	Messages        []Message
	MaxOutputTokens int
}

// Replay serves the supplied sequence once, safely across concurrent callers.
// Canceled or invalid inputs do not consume a step. Exhaustion is explicit.
type Replay struct {
	mu    sync.Mutex
	steps []ReplayStep
	next  int
	calls []Call
}

var _ Provider = (*Replay)(nil)

func NewReplay(steps ...ReplayStep) *Replay {
	return &Replay{steps: append([]ReplayStep(nil), steps...)}
}

func (r *Replay) Complete(ctx context.Context, messages []Message, maxOutputTokens int) (Reply, error) {
	if r == nil || ctx == nil || len(messages) == 0 {
		return Reply{}, ErrInput
	}
	if err := contextFailure(ctx); err != nil {
		return Reply{}, err
	}
	if maxOutputTokens < 1 || maxOutputTokens > HardMaxOutputTokens {
		return Reply{}, ErrOutputLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := contextFailure(ctx); err != nil {
		return Reply{}, err
	}
	r.calls = append(r.calls, Call{Messages: append([]Message(nil), messages...), MaxOutputTokens: maxOutputTokens})
	if r.next >= len(r.steps) {
		return Reply{}, ErrExhausted
	}
	step := r.steps[r.next]
	r.next++
	return step.Reply, step.Err
}

// Calls includes exhausted attempts and returns a fresh copy on every call.
func (r *Replay) Calls() []Call {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := make([]Call, len(r.calls))
	for i, call := range r.calls {
		calls[i] = Call{Messages: append([]Message(nil), call.Messages...), MaxOutputTokens: call.MaxOutputTokens}
	}
	return calls
}
