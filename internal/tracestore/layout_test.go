package tracestore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSiteLayout(t *testing.T) {
	root := t.TempDir()
	s := New(Options{Dir: root})
	fresh := testBatch("new", 1)
	fresh.Site = "site-a"
	mustWrite(t, s, fresh)
	name := batchPath(fresh, 1)
	if len(strings.Split(name, string(filepath.Separator))) != 3 {
		t.Fatal(name)
	}
	if _, err := os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	fresh.Attempt = 2
	mustWrite(t, s, fresh)
	s = New(s.opts)
	got, err := s.List(context.Background(), 7, 500)
	if err != nil || len(got) != 2 || got[1].Attempt != 2 {
		t.Fatal(got, err)
	}
	if err = s.Cleanup(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestRejectUnsupportedLayout(t *testing.T) {
	for _, name := range []string{"00000000000000000001.json", "00000000000000000001.pending"} {
		if _, err := fileSequence(name); err == nil {
			t.Fatal("accepted filename", name)
		}
	}
	path := filepath.Join("07", "task-7", digest("identity"), digest("run"), "00000000000000000001.json")
	if _, _, ok := pathIdentity(path); ok {
		t.Fatal("accepted unsupported layout")
	}
	b := testBatch("run", 1)
	if recordPathValid(record{Version: 1, Batch: b}, entry{path: batchPath(b, 1), seq: 1}) {
		t.Fatal("accepted unsupported record version")
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
