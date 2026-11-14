package tracestore

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cleanup removes records strictly before cutoff by recorded time. It keeps
// pending and corrupt files, reports corruption, and continues cleaning valid
// records. Each record holds the store gate separately so worker publication
// can proceed between records; no active stage is ever removed.
func (s *Store) Cleanup(ctx context.Context, cutoff time.Time) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	r, err := s.open(ctx)
	if err != nil {
		s.release()
		return err
	}
	r.Close()
	entries := make([]entry, 0, len(s.files))
	for _, e := range s.files {
		entries = append(entries, e)
	}
	s.release()
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	var failures []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if !strings.HasSuffix(e.path, ".json") {
			continue
		}
		if err := s.cleanupRecord(ctx, e, cutoff); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (s *Store) cleanupRecord(ctx context.Context, e entry, cutoff time.Time) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if _, exists := s.files[e.path]; !exists {
		return nil
	}
	r, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	rec, err := s.read(ctx, r, e)
	if err != nil {
		return err
	}
	if !rec.RecordedAt.Before(cutoff) {
		return nil
	}
	if err = r.Remove(e.path); err != nil {
		return err
	}
	delete(s.files, e.path)
	s.bytes -= e.size
	for dir := filepath.Dir(e.path); dir != "."; dir = filepath.Dir(dir) {
		if err := r.Remove(dir); err != nil {
			break
		}
	}
	return nil
}
