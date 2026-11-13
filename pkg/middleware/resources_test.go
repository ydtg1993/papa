package middleware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReadLimitedBoundary(t *testing.T) {
	for _, n := range []int{8, 9} {
		b, e := ReadLimited(strings.NewReader(strings.Repeat("x", n)), 8)
		if n == 8 && (e != nil || len(b) != 8) {
			t.Fatal(b, e)
		}
		if n == 9 && !errors.Is(e, ErrResourceLimit) {
			t.Fatal(e)
		}
	}
}
func TestSharedMemoryAdmissionConcurrent(t *testing.T) {
	b := NewDownloadBudget(8, 100)
	release, e := b.AcquireMemory(8)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := b.AcquireMemory(1); !errors.Is(e, ErrResourceLimit) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	release()
	release()
	release, e = b.AcquireMemory(8)
	if e != nil {
		t.Fatal(e)
	}
	release()
}
func TestDiskAccountsRetainedAcrossRootsAndCancellation(t *testing.T) {
	a, c := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(a, "retained"), []byte("12345"), 0600)
	os.WriteFile(filepath.Join(c, "retained"), []byte("12345"), 0600)
	b := NewDownloadBudget(100, 10)
	release, e := b.AcquireDisk(a, 5)
	if e != nil {
		t.Fatal(e)
	}
	release()
	release, e = b.AcquireDisk(c, 0)
	if e != nil {
		t.Fatal(e)
	}
	release()
	if _, e = b.AcquireDisk(c, 1); !errors.Is(e, ErrResourceLimit) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = b.AcquireDiskContext(ctx, a, 0); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestCheckpointCoalescesAndDrainsFinalState(t *testing.T) {
	var writes atomic.Int32
	var value atomic.Int32
	var saved int32
	c := NewCheckpoint(func() error { writes.Add(1); saved = value.Load(); return nil })
	for i := int32(1); i <= 1000; i++ {
		value.Store(i)
		c.Notify()
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	if saved != 1000 || writes.Load() > 2 {
		t.Fatal(saved, writes.Load())
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
}
func TestCheckpointErrorSurvivesDrain(t *testing.T) {
	want := errors.New("disk full")
	c := NewCheckpoint(func() error { return want })
	c.Notify()
	if !errors.Is(c.Close(), want) {
		t.Fatal("lost checkpoint failure")
	}
}
