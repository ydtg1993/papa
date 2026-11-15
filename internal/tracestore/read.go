package tracestore

import (
	"context"
	"sort"
	"strings"

	"github.com/ydtg1993/papa/v3/core"
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
	var latest entry
	for _, e := range s.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, _, ok := pathIdentity(e.path)
		if ok && id == taskID && strings.HasSuffix(e.path, ".json") && e.seq > latest.seq {
			latest = e
		}
	}
	if latest.path == "" {
		return []core.TraceExecutionStep{}, nil
	}
	latestRecord, err := s.read(ctx, r, latest)
	if err != nil {
		return nil, err
	}
	_, execution, _ := pathIdentity(latest.path)
	var entries []entry
	for _, e := range s.files {
		id, exec, ok := pathIdentity(e.path)
		if ok && id == taskID && exec == execution && strings.HasSuffix(e.path, ".json") {
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
		if rec.IdentityHash != latestRecord.IdentityHash || rec.ExecutionID != latestRecord.ExecutionID || (rec.Site != "" && latestRecord.Site != "" && rec.Site != latestRecord.Site) {
			continue
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
