package middleware

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutputLocksCanonicalPaths(t *testing.T) {
	path := filepath.Join("output", "file.bin")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{abs, filepath.Dir(abs) + string(filepath.Separator) + "." + string(filepath.Separator) + "file.bin"}
	if runtime.GOOS == "windows" {
		aliases = append(aliases, strings.ToUpper(abs))
	}
	var locks OutputLocks
	unlock, err := locks.Lock(context.Background(), path, abs)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		release, err := locks.Lock(ctx, alias)
		cancel()
		if release != nil {
			release()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("alias %q bypassed active path lock: %v", alias, err)
		}
	}
	unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("finished/canceled callers retained %d lock entries", len(locks.entries))
	}
}

func TestOutputLocksCancellationReleasesPartialAcquisition(t *testing.T) {
	var locks OutputLocks
	unlock, err := locks.Lock(context.Background(), "z")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	// A blocked resource set must not retain its otherwise available path.
	if release, err := locks.Lock(ctx, "z", "a"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("multi-path wait returned %v", err)
	}
	ctxA, cancelA := context.WithTimeout(context.Background(), time.Second)
	defer cancelA()
	releaseA, err := locks.Lock(ctxA, "a")
	if err != nil {
		t.Fatalf("canceled acquisition retained its earlier path: %v", err)
	}
	releaseA()
	if len(locks.entries) != 1 {
		t.Fatalf("cancellation removed active entry or leaked waiter: entries=%d", len(locks.entries))
	}
}

func TestOutputLocksAncestorPathsConflict(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "movie")
	child := filepath.Join(dir, "nested", "segment.ts")
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	for _, tc := range []struct{ name, held, waiting string }{
		{"directory-first", dir, child},
		{"file-first", child, dir},
		{"volume-root", volumeRoot, child},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var locks OutputLocks
			release, err := locks.Lock(context.Background(), tc.held)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			waitingRelease, waitErr := locks.Lock(ctx, tc.waiting)
			cancel()
			if waitingRelease != nil {
				waitingRelease()
			}
			release()
			if !errors.Is(waitErr, context.DeadlineExceeded) {
				t.Errorf("overlapping path bypassed active lock: held=%q waiting=%q err=%v", tc.held, tc.waiting, waitErr)
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err = locks.Lock(ctx, tc.waiting)
			if err != nil {
				t.Fatal("canceled waiter retained a lock:", err)
			}
			release()
			if len(locks.entries) != 0 {
				t.Fatal("lock entries remain after release")
			}
		})
	}
}

func TestOutputLocksSiblingPathsRemainConcurrent(t *testing.T) {
	root := t.TempDir()
	for _, pair := range [][2]string{{"movie/a.ts", "movie/b.ts"}, {"movie", "movie-other/a.ts"}} {
		var locks OutputLocks
		release, err := locks.Lock(context.Background(), filepath.Join(root, pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		otherRelease, err := locks.Lock(ctx, filepath.Join(root, pair[1]))
		cancel()
		if otherRelease != nil {
			otherRelease()
		}
		release()
		if err != nil {
			t.Fatalf("independent paths serialized: %v: %v", pair, err)
		}
	}
}

func TestOutputLocksConcurrentOverlappingSets(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	sets := [][]string{{a, filepath.Join(b, "part")}, {filepath.Join(a, "part"), b}, {b, a}, {a, a, filepath.Join(a, "part"), b}}
	var locks OutputLocks
	var active atomic.Int32
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	for _, paths := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 20; i++ {
				release, err := locks.Lock(ctx, paths...)
				if err != nil {
					t.Error("overlapping batch deadlocked or failed:", err)
					return
				}
				if n := active.Add(1); n != 1 {
					t.Errorf("conflicting resource sets entered concurrently: %d", n)
				}
				runtime.Gosched()
				active.Add(-1)
				release()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(locks.entries) != 0 {
		t.Fatal("batch release left active locks")
	}
}
