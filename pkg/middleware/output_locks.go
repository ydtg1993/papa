package middleware

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// OutputLocks serializes downloads with overlapping output or resume paths.
// Equal paths and ancestor/descendant paths conflict; siblings can run together.
// Its zero value is ready to use; downloader With variants share the instance.
// Paths are made absolute and cleaned, with case folding on Windows. Symlink and
// hard-link aliases are not resolved, and separate instances/processes do not
// coordinate with one another.
type OutputLocks struct {
	mu      sync.Mutex
	entries map[string]struct{} // only active resources, never waiting requests
	changed chan struct{}       // broadcast when a resource set is released
}

// Lock acquires a complete resource set atomically, so a waiter never retains
// partial locks while waiting for an ancestor or another resource. Waiting can
// be canceled. The returned unlock function must be called exactly once.
func (l *OutputLocks) Lock(ctx context.Context, paths ...string) (func(), error) {
	keys := make([]string, 0, len(paths))
	for _, path := range paths {
		key, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	unique := keys[:0]
	for i, key := range keys {
		if i == 0 || key != keys[i-1] {
			unique = append(unique, key)
		}
	}
	keys = unique
	for {
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		if !l.conflicts(keys) {
			if l.entries == nil {
				l.entries = make(map[string]struct{})
			}
			for _, key := range keys {
				l.entries[key] = struct{}{}
			}
			l.mu.Unlock()
			release := func() { l.release(keys) }
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// conflicts is called with mu held. Only requested resources participate: the
// common parent of two sibling files is not itself locked.
func (l *OutputLocks) conflicts(keys []string) bool {
	for _, key := range keys {
		for held := range l.entries {
			if key == held || outputPathBelow(key, held) || outputPathBelow(held, key) {
				return true
			}
		}
	}
	return false
}

func outputPathBelow(path, parent string) bool {
	separator := string(filepath.Separator)
	if !strings.HasSuffix(parent, separator) {
		parent += separator
	}
	return strings.HasPrefix(path, parent)
}

func (l *OutputLocks) release(keys []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		delete(l.entries, key)
	}
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}
