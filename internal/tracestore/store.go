// Package tracestore owns bounded, immutable trace files for one process.
// It depends only on standard library facilities and core trace value types.
package tracestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

const (
	DefaultMaxFileBytes  int64 = 4 << 20
	DefaultMaxTotalBytes int64 = 256 << 20
	DefaultMaxFiles            = 10000
)

// Options limits new writes. Discovery and reads retain a safety ceiling of at
// least the defaults, allowing cleanup after quotas are reduced. Existing data
// beyond max(configured, default) ceilings requires raising the configured limits.
// Dir must be dedicated to traces and cannot contain symbolic links.
type Options struct {
	Dir                         string
	MaxFileBytes, MaxTotalBytes int64
	MaxFiles                    int
}
type Batch struct {
	TaskID       int              `json:"task_id"`
	IdentityHash string           `json:"identity_hash"`
	ExecutionID  string           `json:"execution_id"`
	Attempt      int              `json:"attempt"`
	RecordedAt   time.Time        `json:"recorded_at"`
	Truncated    bool             `json:"truncated,omitempty"`
	Steps        []core.TraceStep `json:"steps"`
}
type record struct {
	Version int `json:"version"`
	Batch
}
type entry struct {
	path string
	size int64
	seq  uint64
}
type Store struct {
	opts        Options
	gate        chan struct{}
	initialized bool
	files       map[string]entry
	bytes       int64
	sequence    uint64
}

// New does no I/O. Configuration errors are returned by operations.
// A root must have a single Store owner; independent writers are unsupported.
func New(o Options) *Store {
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	return &Store{opts: o, gate: make(chan struct{}, 1)}
}
func (s *Store) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.gate <- struct{}{}:
		return nil
	}
}
func (s *Store) release()    { <-s.gate }
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func taskPath(id int) string {
	return filepath.Join(fmt.Sprintf("%02x", id&255), fmt.Sprintf("task-%d", id))
}
func batchDir(b Batch) string {
	return filepath.Join(taskPath(b.TaskID), identityComponent(b.IdentityHash), digest(b.ExecutionID))
}
func identityComponent(value string) string {
	if len(value) == 64 && value == strings.ToLower(value) {
		if _, err := hex.DecodeString(value); err == nil {
			return value
		}
	}
	return digest(value)
}
func (s *Store) open(ctx context.Context) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.opts.Dir) == "" || s.opts.MaxFileBytes < 1 || s.opts.MaxTotalBytes < 1 || s.opts.MaxFiles < 1 {
		return nil, errors.New("tracestore: invalid options")
	}
	p, err := filepath.Abs(s.opts.Dir)
	if err != nil {
		return nil, err
	}
	// Reject links in the configured root itself, including its existing ancestors.
	for q := p; ; q = filepath.Dir(q) {
		info, e := os.Lstat(q)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("tracestore: symlink root %s", q)
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if filepath.Dir(q) == q {
			break
		}
	}
	if err = os.MkdirAll(p, 0700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, err
	}
	if !s.initialized {
		if err = s.inventory(ctx, r); err != nil {
			r.Close()
			return nil, err
		}
	}
	return r, nil
}
