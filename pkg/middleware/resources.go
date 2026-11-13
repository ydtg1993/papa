package middleware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ErrResourceLimit identifies local resource admission failures. Retrying without
// releasing resources or increasing the configured limit cannot fix this error.
var ErrResourceLimit = errors.New("download resource limit exceeded")

// DownloadLimits are per-download limits. Nonpositive values use finite defaults.
// Public downloader Config layouts are deliberately unchanged.
type DownloadLimits struct {
	ResumeBytes, PlaylistBytes, MediaBytes, InitBytes, KeyBytes, OutputBytes int64
	Chunks, Segments                                                         int
}

func (l DownloadLimits) Normalized() DownloadLimits {
	if l.ResumeBytes <= 0 {
		l.ResumeBytes = 16 << 20
	}
	if l.PlaylistBytes <= 0 {
		l.PlaylistBytes = 8 << 20
	}
	if l.MediaBytes <= 0 {
		l.MediaBytes = 64 << 20
	}
	if l.InitBytes <= 0 {
		l.InitBytes = 16 << 20
	}
	if l.KeyBytes <= 0 {
		l.KeyBytes = 16
	}
	if l.OutputBytes <= 0 {
		l.OutputBytes = 8 << 30
	}
	if l.Chunks <= 0 {
		l.Chunks = 100000
	}
	if l.Segments <= 0 {
		l.Segments = 100000
	}
	// Keep reservation arithmetic representable even for malformed configuration.
	l.ResumeBytes = min(l.ResumeBytes, 1<<50)
	l.PlaylistBytes = min(l.PlaylistBytes, 1<<50)
	l.MediaBytes = min(l.MediaBytes, 1<<50)
	l.InitBytes = min(l.InitBytes, 1<<50)
	l.KeyBytes = min(l.KeyBytes, 1<<50)
	l.OutputBytes = min(l.OutputBytes, 1<<50)
	l.Chunks = min(l.Chunks, 1<<30)
	l.Segments = min(l.Segments, 1<<30)
	return l
}
func ResourceLimit(role string, limit int64) error {
	return fmt.Errorf("%w: %s 超过上界 (limit %d)", ErrResourceLimit, role, limit)
}
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit == int64(^uint64(0)>>1) {
		return nil, ResourceLimit("invalid read limit", limit)
	}
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, ResourceLimit("input bytes", limit)
	}
	return b, nil
}
func ReadLimitedFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if s, e := f.Stat(); e != nil {
		return nil, e
	} else if s.Size() > limit {
		return nil, ResourceLimit("resume bytes", limit)
	}
	return ReadLimited(f, limit)
}

// DownloadBudget is shared by With clones and can be injected into independently
// constructed downloaders. Admission is fail-fast, never waits while holding a
// worker or output lock. Memory is a reservation for live operation buffers, not RSS.
type DownloadBudget struct {
	mu                                 sync.Mutex
	memory, disk, usedMemory, usedDisk int64
	diskGate                           chan struct{}
	roots                              map[string]struct{}
}

func NewDownloadBudget(memoryBytes, diskBytes int64) *DownloadBudget {
	if memoryBytes <= 0 {
		memoryBytes = 1 << 30
	}
	if diskBytes <= 0 {
		diskBytes = 64 << 30
	}
	return &DownloadBudget{memory: memoryBytes, disk: diskBytes, diskGate: make(chan struct{}, 1), roots: make(map[string]struct{})}
}
func (b *DownloadBudget) AcquireMemory(n int64) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.memory-b.usedMemory {
		return nil, ResourceLimit("shared memory", b.memory)
	}
	b.usedMemory += n
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.usedMemory -= n; b.mu.Unlock() }) }, nil
}

// AcquireDisk includes existing retained files below root. It intentionally
// overcounts active files against reservations; it never deletes active/resume data.
// External writers and output retention outside root require filesystem quotas.
func (b *DownloadBudget) AcquireDisk(root string, n int64) (func(), error) {
	return b.AcquireDiskContext(context.Background(), root, n)
}
func (b *DownloadBudget) AcquireDiskContext(ctx context.Context, root string, n int64) (func(), error) {
	select {
	case b.diskGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.diskGate }()
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, ok := b.roots[root]; !ok {
		if len(b.roots) >= 1024 {
			return nil, ResourceLimit("disk roots", 1024)
		}
		b.roots[root] = struct{}{}
	}
	var existing int64
	entries := 0
	for root := range b.roots {
		err := scanDisk(ctx, root, 0, &entries, &existing, b.disk)
		if err != nil {
			return nil, err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || existing > b.disk-b.usedDisk || n > b.disk-b.usedDisk-existing {
		return nil, ResourceLimit("shared temporary disk", b.disk)
	}
	b.usedDisk += n
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.usedDisk -= n; b.mu.Unlock() }) }, nil
}

// scanDisk reads directory entries in fixed batches; WalkDir would allocate an
// entire attacker-sized directory before the entry-limit callback runs.
func scanDisk(ctx context.Context, path string, depth int, entries *int, bytes *int64, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return ResourceLimit("disk inventory depth", 128)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	*entries++
	if *entries > 500000 {
		return ResourceLimit("disk inventory entries", 500000)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("resume disk inventory refuses symlink: %s", path)
	}
	if !info.IsDir() {
		if info.Size() > limit-*bytes {
			return ResourceLimit("retained disk", limit)
		}
		*bytes += info.Size()
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		batch, err := f.ReadDir(128)
		for _, entry := range batch {
			if e := scanDisk(ctx, filepath.Join(path, entry.Name()), depth+1, entries, bytes, limit); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
