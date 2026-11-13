package tracestore

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
)

// List returns the tail of the latest published execution, across its attempts.
// A corrupt latest execution is an error, never an excuse to show stale history.
func (s *Store) List(ctx context.Context, taskID, limit int) ([]core.TraceExecutionStep, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	r, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	prefix := taskPath(taskID) + string(filepath.Separator)
	var latest entry
	for _, e := range s.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(e.path, prefix) && strings.HasSuffix(e.path, ".json") && e.seq > latest.seq {
			latest = e
		}
	}
	if latest.path == "" {
		return []core.TraceExecutionStep{}, nil
	}
	dir := filepath.Dir(latest.path)
	var entries []entry
	for _, e := range s.files {
		if filepath.Dir(e.path) == dir && strings.HasSuffix(e.path, ".json") {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]core.TraceExecutionStep, 0, limit)
	truncated := false
	for _, e := range entries {
		rec, err := s.read(ctx, r, e)
		if err != nil {
			return nil, err
		}
		truncated = truncated || rec.Truncated
		for _, step := range rec.Steps {
			step.Attempt = rec.Attempt
			out = append(out, core.TraceExecutionStep{TraceStep: step, ExecutionID: rec.ExecutionID})
		}
		if len(out) > limit {
			out = append([]core.TraceExecutionStep(nil), out[len(out)-limit:]...)
			truncated = true
		}
	}
	if len(out) > 0 {
		out[0].Truncated = truncated
	}
	return out, nil
}
