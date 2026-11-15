package tracestore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
)

func snapshotToken(path string, index int) string {
	return digest(filepath.ToSlash(path)) + "." + strconv.Itoa(index)
}

// encodeBatch checks source sizes before JSON allocation. Snapshots are optional:
// dropping them must never prevent an otherwise fitting trace from being
// published. At the step ceiling, reserve one slot for snapshot references by
// removing the oldest ordinary step (or oldest step if none is ordinary).
// The caller's slices are never modified.
func (s *Store) encodeBatch(ctx context.Context, b Batch, path string, limit int64) ([]byte, error) {
	source := b.Snapshots
	originalSteps := b.Steps
	b.Snapshots = nil
	if len(b.Steps) == 500 && len(source) > 0 {
		// Keep the latest failure and its page together. Prefer sacrificing the
		// oldest ordinary step; if all steps failed, sacrifice the oldest one.
		drop := 0
		for i, step := range b.Steps {
			if step.Status == "ok" {
				drop = i
				break
			}
		}
		steps := make([]core.TraceStep, 0, 499)
		steps = append(steps, b.Steps[:drop]...)
		b.Steps = append(steps, b.Steps[drop+1:]...)
		b.Truncated = true
	}
	remaining := s.opts.MaxFileBytes
	remaining -= int64(len(b.IdentityHash)) + int64(len(b.ExecutionID))
	for _, step := range b.Steps {
		remaining -= int64(len(step.Step)) + int64(len(step.Status)) + int64(len(step.Kind)) + int64(len(step.Message)) + int64(len(step.Data))
	}
	if len(source) > 32 {
		b.Truncated = true
	}
	if len(b.Steps) < 500 {
		for i := len(source) - 1; i >= max(0, len(source)-32); i-- {
			snap := source[i]
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			n := int64(len(snap.Body)) + int64(len(snap.URL)) + int64(len(snap.ContentType))
			if n > remaining {
				b.Truncated = true
				continue
			}
			remaining -= n
			b.Snapshots = append(b.Snapshots, snap)
		}
		// Select recent pages first under the source budget, then publish them
		// in their original chronological order.
		for i, j := 0, len(b.Snapshots)-1; i < j; i, j = i+1, j-1 {
			b.Snapshots[i], b.Snapshots[j] = b.Snapshots[j], b.Snapshots[i]
		}
	}
	steps := b.Steps
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.Steps = steps
		if len(b.Snapshots) == 0 {
			b.Steps = originalSteps
		}
		if len(b.Snapshots) > 0 {
			files := make([]string, len(b.Snapshots))
			for i := range files {
				files[i] = snapshotToken(path, i)
			}
			refs, _ := json.Marshal(struct {
				Files []string `json:"files"`
			}{files})
			seq := 1
			for _, step := range steps {
				if step.Seq >= seq {
					seq = step.Seq + 1
				}
			}
			b.Steps = append(append([]core.TraceStep(nil), steps...), core.TraceStep{
				Attempt: b.Attempt, Seq: seq, Step: "页面快照", Status: "ok", Data: string(refs), CreatedAt: b.RecordedAt,
			})
		}
		data, err := json.Marshal(record{Version: 2, Batch: b})
		if err != nil {
			return nil, err
		}
		if int64(len(data)) <= limit {
			return data, nil
		}
		if len(b.Snapshots) == 0 {
			return nil, errors.New("tracestore: batch exceeds file or storage quota limit")
		}
		b.Snapshots = b.Snapshots[1:]
		b.Truncated = true
	}
}

// Snapshot resolves an opaque token emitted in a 页面快照 step's Data.files.
// Tokens consist of a record-path SHA-256 digest and a zero-based page index.
// No token component is ever used as a filesystem path.
func (s *Store) Snapshot(ctx context.Context, token string) ([]byte, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if len(token) < 66 || len(token) > 67 {
		return nil, errors.New("tracestore: invalid snapshot token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) > 2 {
		return nil, errors.New("tracestore: invalid snapshot token")
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return nil, errors.New("tracestore: invalid snapshot token")
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 0 || index >= 32 || strconv.Itoa(index) != parts[1] {
		return nil, errors.New("tracestore: invalid snapshot index")
	}
	r, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for _, e := range s.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(e.path, ".json") && snapshotToken(e.path, index) == token {
			rec, err := s.read(ctx, r, e)
			if err != nil {
				return nil, err
			}
			if index >= len(rec.Snapshots) {
				return nil, errors.New("tracestore: snapshot index out of range")
			}
			return rec.Snapshots[index].Body, nil
		}
	}
	return nil, fmt.Errorf("tracestore: snapshot not found")
}
