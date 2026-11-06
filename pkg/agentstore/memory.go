package agentstore

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a detached, concurrency-safe store for tests and ephemeral runs.
// Only MySQLStore survives a process restart.
type MemoryStore struct {
	mu    sync.Mutex
	runs  map[string]runState
	steps map[string][]Step
}

var _ Store = (*MemoryStore)(nil)

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: make(map[string]runState), steps: make(map[string][]Step)}
}

func (s *MemoryStore) Start(ctx context.Context, run Run) (Run, error) {
	run, err := prepareStart(run)
	if err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	if _, exists := s.runs[run.ID]; exists {
		return Run{}, ErrConflict
	}
	if s.runs == nil {
		s.runs = make(map[string]runState)
		s.steps = make(map[string][]Step)
	}
	run.CreatedAt = time.Now().UTC()
	run.UpdatedAt = run.CreatedAt
	s.runs[run.ID] = runState{run: run}
	return copyRun(run), nil
}

func (s *MemoryStore) mutate(ctx context.Context, id string, revision uint64, op operation) (Run, Step, error) {
	if err := mutationKey(ctx, id, revision); err != nil {
		return Run{}, Step{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Run{}, Step{}, err
	}
	state, exists := s.runs[id]
	if !exists {
		return Run{}, Step{}, ErrNotFound
	}
	var pending *Step
	if state.pending != 0 {
		if state.pending > uint64(len(s.steps[id])) {
			return Run{}, Step{}, ErrCorrupt
		}
		p := copyStep(s.steps[id][state.pending-1])
		pending = &p
	}
	next, step, err := change(state, pending, revision, op, time.Now().UTC())
	if err != nil {
		return Run{}, Step{}, err
	}
	s.runs[id] = next
	var out Step
	if step != nil {
		out = copyStep(*step)
		if op.kind == "begin" {
			s.steps[id] = append(s.steps[id], *step)
		} else {
			s.steps[id][step.Sequence-1] = *step
		}
	}
	return copyRun(next.run), out, nil
}

func (s *MemoryStore) BeginStep(ctx context.Context, id string, revision uint64, step Step, usage string) (Run, Step, error) {
	return s.mutate(ctx, id, revision, operation{kind: "begin", step: step, usage: usage})
}

func (s *MemoryStore) EndStep(ctx context.Context, id string, revision uint64, step Step, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "end", step: step, usage: usage})
	return r, err
}

func (s *MemoryStore) UpdateUsage(ctx context.Context, id string, revision uint64, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "usage", usage: usage})
	return r, err
}

func (s *MemoryStore) Finish(ctx context.Context, id string, revision uint64, status, result, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "finish", status: status, result: result, usage: usage})
	return r, err
}

func (s *MemoryStore) Interrupt(ctx context.Context, id string, revision uint64) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "interrupt"})
	return r, err
}

func (s *MemoryStore) Get(ctx context.Context, id string) (Run, error) {
	if err := readKey(ctx, id); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	r, exists := s.runs[id]
	if !exists {
		return Run{}, ErrNotFound
	}
	return copyRun(r.run), nil
}

// Steps returns the earliest steps in sequence order, bounded by limit (0=256).
func (s *MemoryStore) Steps(ctx context.Context, id string, limit int) ([]Step, error) {
	if err := readKey(ctx, id); err != nil {
		return nil, err
	}
	limit, err := checkedLimit(limit)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, exists := s.runs[id]; !exists {
		return nil, ErrNotFound
	}
	items := s.steps[id]
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]Step, len(items))
	for i := range items {
		out[i] = copyStep(items[i])
	}
	return out, nil
}
