package tracestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

func testBatch(exec string, attempt int) Batch {
	return Batch{TaskID: 7, IdentityHash: "identity", ExecutionID: exec, Attempt: attempt, RecordedAt: time.Now().UTC(), Steps: []core.TraceStep{{Seq: 1, Step: "fetch", Status: "failed", Message: exec}}}
}
func mustWrite(t *testing.T, s *Store, b Batch) {
	t.Helper()
	if err := s.Write(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}
func TestLatestExecutionRestartAndTail(t *testing.T) {
	o := Options{Dir: t.TempDir()}
	s := New(o)
	b := testBatch("z-old", 1)
	mustWrite(t, s, b)
	b = testBatch("a-new", 1)
	b.Truncated = true
	mustWrite(t, s, b)
	b = testBatch("a-new", 2)
	mustWrite(t, s, b)
	for _, store := range []*Store{s, New(o)} {
		got, err := store.List(context.Background(), 7, 1)
		if err != nil || len(got) != 1 || got[0].ExecutionID != "a-new" || got[0].Attempt != 2 || !got[0].Truncated {
			t.Fatalf("got %#v, %v", got, err)
		}
	}
	b = testBatch("0-newest", 1)
	mustWrite(t, New(o), b)
	got, err := New(o).List(context.Background(), 7, 500)
	if err != nil || len(got) != 1 || got[0].ExecutionID != "0-newest" {
		t.Fatalf("got %#v %v", got, err)
	}
}
func TestQuotaCancelAndNoPartialPublication(t *testing.T) {
	s := New(Options{Dir: t.TempDir(), MaxFiles: 1})
	mustWrite(t, s, testBatch("first", 1))
	if err := s.Write(context.Background(), testBatch("second", 1)); err == nil {
		t.Fatal("quota accepted")
	}
	got, err := s.List(context.Background(), 7, 500)
	if err != nil || len(got) != 1 || got[0].ExecutionID != "first" {
		t.Fatal(got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Write(ctx, testBatch("cancel", 1)); err == nil {
		t.Fatal("cancel ignored")
	}
	s = New(Options{Dir: t.TempDir(), MaxFileBytes: 200})
	if err := s.Write(context.Background(), testBatch("large", 1)); err == nil {
		t.Fatal("size limit ignored")
	}
	s = New(Options{Dir: t.TempDir(), MaxTotalBytes: 100})
	if err := s.Write(context.Background(), testBatch("large", 1)); err == nil {
		t.Fatal("total limit ignored")
	}
	// Force rename failure with a directory at the destination. The stage is removed.
	s = New(Options{Dir: t.TempDir()})
	r, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := testBatch("rename", 1)
	name := filepath.Join(batchDir(b), "00000000000000000001.json")
	if err := r.MkdirAll(name, 0700); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := s.Write(context.Background(), b); err == nil {
		t.Fatal("rename should fail")
	}
	if len(s.files) != 0 || s.bytes != 0 {
		t.Fatal("failed publication changed inventory")
	}
	pending := filepath.Join(s.opts.Dir, strings.TrimSuffix(name, ".json")+".pending")
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("stage leaked", err)
	}
}
func TestRetentionUsesRecordTimeAndKeepsPending(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	old := testBatch("old", 1)
	old.RecordedAt = time.Now().Add(-8 * 24 * time.Hour)
	mustWrite(t, s, old)
	fresh := testBatch("fresh", 1)
	mustWrite(t, s, fresh)
	for _, e := range s.files {
		_ = os.Chtimes(filepath.Join(s.opts.Dir, e.path), time.Unix(1, 0), time.Unix(1, 0))
	}
	if err := s.Cleanup(context.Background(), time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(s.files) != 1 {
		t.Fatal("wrong files removed")
	}
	got, err := New(s.opts).List(context.Background(), 7, 10)
	if err != nil || len(got) != 1 || got[0].ExecutionID != "fresh" {
		t.Fatal(got, err)
	}
	pending := filepath.Join(s.opts.Dir, batchDir(fresh), "00000000000000000003.pending")
	if err := os.WriteFile(pending, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.opts)
	if err := restarted.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("pending removed", err)
	}
	if len(restarted.files) != 1 {
		t.Fatal("pending not counted")
	}
}
func TestCorruptLatestDoesNotFallBack(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	mustWrite(t, s, testBatch("old", 1))
	mustWrite(t, s, testBatch("new", 1))
	for _, e := range s.files {
		if e.seq == 2 {
			if err := os.WriteFile(filepath.Join(s.opts.Dir, e.path), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, store := range []*Store{s, New(s.opts)} {
		if _, err := store.List(context.Background(), 7, 500); err == nil {
			t.Fatal("corruption hidden")
		}
	}
}
func TestConcurrentStoreOperations(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Write(context.Background(), testBatch(fmt.Sprintf("e-%d", i), 1)); err != nil {
				t.Error(err)
			}
			if _, err := s.List(context.Background(), 7, 500); err != nil {
				t.Error(err)
			}
			if err := s.Cleanup(context.Background(), time.Unix(0, 0)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(s.files) != 20 {
		t.Fatal(len(s.files))
	}
}
func TestUntrustedNamesAndSymlink(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("../../outside", 1)
	b.IdentityHash = "../../outside"
	mustWrite(t, s, b)
	if _, err := s.List(context.Background(), 7, 500); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := New(Options{Dir: link}).Write(context.Background(), testBatch("x", 1)); err == nil {
		t.Fatal("root symlink accepted")
	}
	if _, err := New(Options{Dir: root}).List(context.Background(), 7, 500); err == nil {
		t.Fatal("inner symlink accepted")
	}
}
func TestCanceledLockWait(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.List(ctx, 7, 500); err == nil {
		t.Fatal("lock wait ignored cancellation")
	}
	s.release()
	mustWrite(t, s, testBatch("after", 1))
}

func TestCleanupContinuesAfterCorruptRecord(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	mustWrite(t, s, testBatch("corrupt", 1))
	mustWrite(t, s, testBatch("valid-expired", 1))
	for _, e := range s.files {
		if e.seq == 1 {
			if err := os.WriteFile(filepath.Join(s.opts.Dir, e.path), []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Cleanup(context.Background(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("corruption not reported")
	}
	if len(s.files) != 1 {
		t.Fatalf("valid expired record retained: %d", len(s.files))
	}
	for _, e := range s.files {
		if e.seq != 1 {
			t.Fatal("corrupt record deleted")
		}
	}
}
func TestLoweredQuotasAllowCleanup(t *testing.T) {
	o := Options{Dir: t.TempDir()}
	s := New(o)
	mustWrite(t, s, testBatch("one", 1))
	mustWrite(t, s, testBatch("two", 1))
	o.MaxFiles = 1
	o.MaxFileBytes = 10
	o.MaxTotalBytes = 10
	s = New(o)
	if err := s.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(s.files) != 0 || s.bytes != 0 {
		t.Fatal("lowered quota blocked cleanup")
	}
}
func TestCanonicalIdentityDirectory(t *testing.T) {
	b := testBatch("execution", 1)
	b.IdentityHash = strings.Repeat("ab", 32)
	if got := filepath.Base(filepath.Dir(batchDir(b))); got != b.IdentityHash {
		t.Fatalf("identity double-hashed: %s", got)
	}
	s := New(Options{Dir: t.TempDir()})
	mustWrite(t, s, b)
	if _, err := New(s.opts).List(context.Background(), 7, 500); err != nil {
		t.Fatal(err)
	}
}
