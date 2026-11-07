package agentstore

import (
	"context"
	"encoding/json"
	"sort"
)

func (s *MemoryStore) List(ctx context.Context, f RunFilter) (RunPage, error) {
	out := RunPage{Items: []Run{}}
	if ctx == nil {
		return out, ErrInvalid
	}
	if err := f.Validate(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []Run
	for _, state := range s.runs {
		r := state.run
		if !r.CreatedAt.Before(f.From) && r.CreatedAt.Before(f.Until) && (f.Before == "" || r.ID < f.Before) && (f.Status == "" || r.Status == f.Status) {
			candidates = append(candidates, copyRun(r))
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID > candidates[j].ID })
	if len(candidates) > 50 {
		candidates = candidates[:50]
		out.Next = candidates[49].ID
	}
	out.Scanned = len(candidates)
	for _, r := range candidates {
		var p struct{ Goal struct{ Site string } }
		if json.Unmarshal([]byte(r.SnapshotJSON), &p) != nil {
			return out, ErrCorrupt
		}
		if f.Site == "" || p.Goal.Site == f.Site {
			out.Items = append(out.Items, r)
		}
	}
	return out, nil
}
func (s *MemoryStore) Inspect(ctx context.Context, id string, after uint64) (RunDetail, error) {
	out := RunDetail{Steps: []Step{}}
	if ctx == nil {
		return out, ErrInvalid
	}
	if err := readKey(ctx, id); err != nil {
		return out, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.runs[id]
	if !ok {
		return out, ErrNotFound
	}
	out.Run = copyRun(state.run)
	for _, step := range s.steps[id] {
		if step.Sequence <= after {
			continue
		}
		if len(out.Steps) == 100 {
			out.Next = out.Steps[99].Sequence
			break
		}
		out.Steps = append(out.Steps, copyStep(step))
	}
	return out, nil
}
