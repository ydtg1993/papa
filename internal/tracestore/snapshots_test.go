package tracestore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

func snapshotRefs(t *testing.T, s *Store) []string {
	t.Helper()
	steps, err := s.List(context.Background(), 7, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.Step == "页面快照" {
			var refs struct {
				Files []string `json:"files"`
			}
			if err := json.Unmarshal([]byte(st.Data), &refs); err != nil {
				t.Fatal(err)
			}
			return refs.Files
		}
	}
	return nil
}

func TestSnapshotByteExactRestartAndCleanup(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("snap", 1)
	body := []byte{0, 255, 254, '<', '>', '\n', 128}
	b.Snapshots = []Snapshot{{URL: "https://example.test", Status: 200, ContentType: "text/html", Body: body}}
	mustWrite(t, s, b)
	for _, store := range []*Store{s, New(s.opts)} {
		refs := snapshotRefs(t, store)
		if len(refs) != 1 {
			t.Fatal(refs)
		}
		got, err := store.Snapshot(context.Background(), refs[0])
		if err != nil || !bytes.Equal(got, body) {
			t.Fatal(got, err)
		}
		if len(store.files) != 1 {
			t.Fatal("snapshot created sidecar")
		}
	}
	token := snapshotRefs(t, s)[0]
	if err := s.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background(), token); err == nil {
		t.Fatal("expired snapshot accessible")
	}
	if len(s.files) != 0 || s.bytes != 0 {
		t.Fatal("cleanup leaked bytes")
	}
}

func TestSnapshotBoundsPreserveFailureSteps(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		opts                   Options
		count, bodySize, steps int
	}{
		{"source", Options{MaxFileBytes: 2048}, 1, 4096, 1},
		{"encoded", Options{MaxFileBytes: 2048}, 1, 1600, 1},
		{"storage", Options{MaxTotalBytes: 2048}, 1, 4096, 1},
		{"count", Options{}, 40, 1, 1},
		{"steps", Options{}, 1, 1, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Dir = t.TempDir()
			s := New(tc.opts)
			b := testBatch(tc.name, 1)
			for len(b.Steps) < tc.steps {
				b.Steps = append(b.Steps, core.TraceStep{Step: "failure", Status: "failed"})
			}
			b.Steps[len(b.Steps)-1].Message = "latest-failure"
			for i := 0; i < tc.count; i++ {
				b.Snapshots = append(b.Snapshots, Snapshot{Body: bytes.Repeat([]byte{'x'}, tc.bodySize)})
			}
			mustWrite(t, s, b)
			got, err := s.List(context.Background(), 7, 500)
			if err != nil || len(got) < tc.steps || !got[0].Truncated || got[0].Status != "failed" {
				t.Fatal(got, err)
			}
			if len(got) > 500 || len(snapshotRefs(t, s)) > 32 {
				t.Fatal("bound exceeded")
			}
			if tc.steps == 500 {
				if len(snapshotRefs(t, s)) != 1 || got[len(got)-2].Message != "latest-failure" {
					t.Fatal("latest failure and its snapshot must both survive the step ceiling")
				}
			}
		})
	}
}

func TestSnapshotStepCeilingDropsOldestOrdinaryStep(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("ordinary", 1)
	for len(b.Steps) < 500 {
		b.Steps = append(b.Steps, core.TraceStep{Status: "failed", Message: "keep-failure"})
	}
	b.Steps[100] = core.TraceStep{Status: "ok", Message: "oldest-ordinary"}
	b.Steps[200] = core.TraceStep{Status: "ok", Message: "later-ordinary"}
	b.Snapshots = []Snapshot{{Body: []byte("latest-page")}}
	mustWrite(t, s, b)
	got, err := s.List(context.Background(), 7, 500)
	if err != nil || len(got) != 500 || !got[0].Truncated || got[0].Message != b.Steps[0].Message {
		t.Fatalf("failure lost: %v %v", got, err)
	}
	foundLater := false
	for _, st := range got {
		if st.Message == "oldest-ordinary" {
			t.Fatal("oldest ordinary step retained")
		}
		foundLater = foundLater || st.Message == "later-ordinary"
	}
	if !foundLater || len(snapshotRefs(t, s)) != 1 {
		t.Fatal("later ordinary step or snapshot lost")
	}
	if b.Steps[100].Message != "oldest-ordinary" {
		t.Fatal("caller slice modified")
	}
}

func TestSnapshotEncodedLimitKeepsLatestAndAttempt(t *testing.T) {
	s := New(Options{Dir: t.TempDir(), MaxFileBytes: 1600})
	b := testBatch("tail", 3)
	b.Snapshots = []Snapshot{{Body: bytes.Repeat([]byte{'a'}, 400)}, {Body: bytes.Repeat([]byte{'z'}, 400)}}
	mustWrite(t, s, b)
	refs := snapshotRefs(t, s)
	if len(refs) != 1 {
		t.Fatalf("want one latest snapshot, got %v", refs)
	}
	body, err := s.Snapshot(context.Background(), refs[0])
	if err != nil || !bytes.Equal(body, b.Snapshots[1].Body) {
		t.Fatalf("latest snapshot lost: %q %v", body, err)
	}
	r, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, e := range s.files {
		rec, err := s.read(context.Background(), r, e)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Steps[len(rec.Steps)-1].Attempt != 3 || !rec.Truncated {
			t.Fatalf("attempt/truncation lost: %+v", rec)
		}
	}
}

func TestSnapshotAtomicPublicationAndCancellation(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("atomic", 1)
	b.Snapshots = []Snapshot{{Body: []byte("body")}}
	r, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := batchPath(b, 1)
	if err := r.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := s.Write(context.Background(), b); err == nil {
		t.Fatal("rename unexpectedly succeeded")
	}
	if len(s.files) != 0 || s.bytes != 0 {
		t.Fatal("failed record published")
	}
	if _, err := s.Snapshot(context.Background(), snapshotToken(path, 0)); err == nil {
		t.Fatal("unpublished snapshot accessible")
	}
	if _, err := os.Stat(filepath.Join(s.opts.Dir, strings.TrimSuffix(path, ".json")+".pending")); !os.IsNotExist(err) {
		t.Fatal("pending leaked", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Write(ctx, b); err == nil {
		t.Fatal("write ignored cancel")
	}
	if _, err := s.Snapshot(ctx, snapshotToken(path, 0)); err == nil {
		t.Fatal("read ignored cancel")
	}
}

func TestSnapshotRejectsTokensAndSymlinks(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("secure", 1)
	b.Snapshots = []Snapshot{{Body: []byte("body")}}
	mustWrite(t, s, b)
	token := snapshotRefs(t, s)[0]
	for _, bad := range []string{"../secret", "C:\\secret", token + ".0", token[:65] + "32", token[:65] + "-1", token[:65] + "01", strings.Repeat("a", 64) + ".0", token[:65] + "1"} {
		if _, err := s.Snapshot(context.Background(), bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	var path string
	for _, e := range s.files {
		path = filepath.Join(s.opts.Dir, e.path)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := s.Snapshot(context.Background(), token); err == nil {
		t.Fatal("symlink followed")
	}
}
