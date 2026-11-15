package tracestore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLegacyFixture(t *testing.T, root string, b Batch, seq uint64) string {
	t.Helper()
	b.Site = ""
	name := filepath.Join(legacyBatchDir(b), fmt.Sprintf("%020d.json", seq))
	data, err := json.Marshal(record{Version: 1, Batch: b})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSiteLayoutAndLegacyCoexistence(t *testing.T) {
	root := t.TempDir()
	old := testBatch("old", 1)
	old.RecordedAt = time.Now().Add(-48 * time.Hour)
	legacy := writeLegacyFixture(t, root, old, 9)
	s := New(Options{Dir: root})
	got, err := s.List(context.Background(), 7, 500)
	if err != nil || len(got) != 1 || got[0].ExecutionID != "old" {
		t.Fatal(got, err)
	}
	fresh := testBatch("new", 1)
	fresh.Site = "site-a"
	mustWrite(t, s, fresh)
	name := batchPath(fresh, 10)
	if len(strings.Split(name, string(filepath.Separator))) != 3 {
		t.Fatal("unexpected layout", name)
	}
	if _, err = os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	fresh.Attempt = 2
	mustWrite(t, s, fresh)
	other := testBatch("another-task", 1)
	other.TaskID = 8
	other.Site = "site-b"
	mustWrite(t, s, other)
	s = New(s.opts)
	got, err = s.List(context.Background(), 7, 500)
	if err != nil || len(got) != 2 || got[0].ExecutionID != "new" || got[1].Attempt != 2 {
		t.Fatal(got, err)
	}
	if err = s.Cleanup(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy retention failed", err)
	}
	if _, err = os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatal("fresh trace removed", err)
	}
	if err = s.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("empty directories retained", entries, err)
	}
}

func TestSiteLayoutRejectsUnsafeSites(t *testing.T) {
	for _, site := range []string{"", ".", "..", "../outside", `a\b`, "/tmp", "a:b", "CON", "lpt1.txt", "trailing.", strings.Repeat("x", 129)} {
		t.Run(site, func(t *testing.T) {
			s := New(Options{Dir: t.TempDir()})
			b := testBatch("run", 1)
			b.Site = site
			if err := s.Write(context.Background(), b); err == nil {
				t.Fatal("unsafe site accepted")
			}
		})
	}
}

func TestSiteAndIdentityDoNotMixExecutions(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("same-execution", 1)
	mustWrite(t, s, b)
	b.IdentityHash = "other-identity"
	b.Attempt = 2
	mustWrite(t, s, b)
	got, err := New(s.opts).List(context.Background(), 7, 500)
	if err != nil || len(got) != 1 || got[0].Attempt != 2 {
		t.Fatal(got, err)
	}
	b.Site = "other-site"
	b.Attempt = 3
	mustWrite(t, s, b)
	got, err = New(s.opts).List(context.Background(), 7, 500)
	if err != nil || len(got) != 1 || got[0].Attempt != 3 {
		t.Fatal(got, err)
	}
}

func TestSiteMetadataMismatchRejected(t *testing.T) {
	s := New(Options{Dir: t.TempDir()})
	b := testBatch("run", 1)
	mustWrite(t, s, b)
	path := filepath.Join(s.opts.Dir, batchPath(b, 1))
	b.Site = "forged"
	data, err := json.Marshal(record{Version: 2, Batch: b})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(s.opts).List(context.Background(), 7, 500); err == nil {
		t.Fatal("site mismatch accepted")
	}
}
