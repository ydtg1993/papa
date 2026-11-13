// Package inputbudget owns bounded report input I/O. Budgets are per operation;
// callers must not share a Meter concurrently.
package inputbudget

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
)

var ErrLimit = errors.New("report input resource limit exceeded")

type LimitError struct {
	Resource string
	Limit    int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: %s limit=%d", ErrLimit, e.Resource, e.Limit)
}
func (e *LimitError) Unwrap() error { return ErrLimit }

// Limits counts physical line bytes (including newline), decompressed scan bytes,
// window-matching event rows (including duplicates), unique key counts, and
// encoded retained/spooled bytes. Event and log families each get a separate
// scan/file budget; these are per-call safeguards, not process-wide admission.
// Zero or negative
// values select defaults; no field disables protection.
type Limits struct {
	MaxFiles                        int
	MaxLineBytes, MaxBytes          int64
	MaxEvents, MaxDedupKeys         int
	MaxRetainedBytes, MaxSpoolBytes int64
	MaxDays                         int
}

func (l Limits) Normalize() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 1024
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = 1 << 20
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 256 << 20
	}
	if l.MaxEvents <= 0 {
		l.MaxEvents = 250000
	}
	if l.MaxDedupKeys <= 0 {
		l.MaxDedupKeys = 250000
	}
	if l.MaxRetainedBytes <= 0 {
		l.MaxRetainedBytes = 64 << 20
	}
	if l.MaxSpoolBytes <= 0 {
		l.MaxSpoolBytes = 512 << 20
	}
	if l.MaxDays <= 0 {
		l.MaxDays = 366
	}
	return l
}
func Check(resource string, n, max int64) error {
	if n > max {
		return &LimitError{resource, max}
	}
	return nil
}

type Meter struct {
	Limits Limits
	Bytes  int64
}

func New(l Limits) *Meter { return &Meter{Limits: l.Normalize()} }

// Reader checks cancellation at every underlying read, including gzip reads.
func Reader(ctx context.Context, r io.Reader) io.Reader { return contextReader{ctx, r} }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Line never allocates more than MaxLineBytes plus one fixed reader buffer.
func (m *Meter) Line(ctx context.Context, r *bufio.Reader) (string, error) {
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		part, err := r.ReadSlice('\n')
		if e := Check("decompressed bytes", int64(len(part)), m.Limits.MaxBytes-m.Bytes); e != nil {
			return "", &LimitError{"decompressed bytes", m.Limits.MaxBytes}
		}
		m.Bytes += int64(len(part))
		if e := Check("line bytes", int64(len(line))+int64(len(part)), m.Limits.MaxLineBytes); e != nil {
			return "", e
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(line), err
	}
}
