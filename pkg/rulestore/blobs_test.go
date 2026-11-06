package rulestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newTestBlobs(t *testing.T) (*FileBlobs, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "samples")
	b, err := NewFileBlobs(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, path
}

func TestFileBlobsConcurrentDedupAndIndependentReads(t *testing.T) {
	b, path := newTestBlobs(t)
	body := []byte("approved sample")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := b.Put(context.Background(), body)
			if err != nil || hash != bodySHA256(body) {
				t.Errorf("Put: %q, %v", hash, err)
			}
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v, %v", entries, err)
	}
	a, err := b.Get(context.Background(), bodySHA256(body), 0)
	if err != nil {
		t.Fatal(err)
	}
	a[0] = 'X'
	got, err := b.Get(context.Background(), bodySHA256(body), 0)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("independent read: %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0700 {
			t.Fatalf("directory mode %o", info.Mode().Perm())
		}
		info, _ = os.Stat(filepath.Join(path, bodySHA256(body)))
		if info.Mode().Perm() != 0600 {
			t.Fatalf("file mode %o", info.Mode().Perm())
		}
	}
}

func TestFileBlobsRejectsCorruptionAndDoesNotOverwrite(t *testing.T) {
	b, path := newTestBlobs(t)
	body := []byte("approved sample")
	hash, err := b.Put(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, hash), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(context.Background(), hash, 0); !errors.Is(err, ErrBlobInvalid) {
		t.Fatalf("corruption: %v", err)
	}
	if _, err := b.Put(context.Background(), body); !errors.Is(err, ErrBlobInvalid) {
		t.Fatalf("duplicate corruption: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(path, hash))
	if string(got) != "corrupt" {
		t.Fatal("corrupt existing file was overwritten")
	}
}

func TestFileBlobsRejectsUnknownNonregularAndEscapingNames(t *testing.T) {
	b, path := newTestBlobs(t)
	for _, hash := range []string{"../sample", strings.Repeat("A", 64), strings.Repeat("g", 64), ""} {
		if _, err := b.Get(context.Background(), hash, 0); !errors.Is(err, ErrBlobInvalid) {
			t.Fatalf("invalid hash %q: %v", hash, err)
		}
	}
	hash := bodySHA256([]byte("missing"))
	if _, err := b.Get(context.Background(), hash, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown: %v", err)
	}
	if err := os.Mkdir(filepath.Join(path, hash), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(context.Background(), hash, 0); !errors.Is(err, ErrBlobInvalid) {
		t.Fatalf("directory: %v", err)
	}
}

func TestFileBlobsRejectsSymlinks(t *testing.T) {
	b, path := newTestBlobs(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := bodySHA256([]byte("secret"))
	if err := os.Symlink(outside, filepath.Join(path, hash)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := b.Get(context.Background(), hash, 0); !errors.Is(err, ErrBlobInvalid) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := b.Put(context.Background(), []byte("secret")); !errors.Is(err, ErrBlobInvalid) {
		t.Fatalf("symlink overwrite: %v", err)
	}
}

func TestFileBlobsBudgetsAndCancellation(t *testing.T) {
	b, _ := newTestBlobs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Put(ctx, []byte("body")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := b.Put(context.Background(), make([]byte, MaxSampleBodyBytes+1)); !errors.Is(err, ErrSampleBudget) {
		t.Fatal(err)
	}
	hash, err := b.Put(context.Background(), []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 1, MaxSampleBodyBytes + 1} {
		if _, err := b.Get(context.Background(), hash, limit); !errors.Is(err, ErrSampleBudget) {
			t.Fatalf("budget %d: %v", limit, err)
		}
	}
	if _, err := b.Get(ctx, hash, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
