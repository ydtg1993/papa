package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func snapshotStoreManifest(id int) ruleSnapshotManifest {
	return ruleSnapshotManifest{Version: 1, EvidenceID: fmt.Sprintf("%032x", id), Site: "test", Stage: "detail", CreatedAt: time.Now()}
}

func snapshotStoreWrite(t *testing.T, root string, id int, body string) (ruleSnapshotManifest, string) {
	t.Helper()
	m, rel, err := writeRuleSnapshot(root, snapshotStoreManifest(id), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return m, rel
}

func snapshotStoreEntries(t *testing.T, root, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ruleSnapshotDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func snapshotStoreAge(t *testing.T, root string, paths ...string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	for _, rel := range paths {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotStoreExists(t *testing.T, root, rel string, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	if want && err != nil || !want && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("exists(%q)=%v, want %v (err=%v)", rel, err == nil, want, err)
	}
}

func TestRuleSnapshotStoreDeduplicatesSanitizedBody(t *testing.T) {
	root := t.TempDir()
	m1, p1 := snapshotStoreWrite(t, root, 1, "<p>sanitized</p>")
	m2, p2 := snapshotStoreWrite(t, root, 2, "<p>sanitized</p>")
	if p1 == p2 || m1.BodyFile != m2.BodyFile || m1.BodySHA256 != m2.BodySHA256 {
		t.Fatalf("observations must differ and bodies must match: %+v %+v", m1, m2)
	}
	if n := len(snapshotStoreEntries(t, root, "observations")); n != 2 {
		t.Fatalf("observations=%d, want 2", n)
	}
	if n := len(snapshotStoreEntries(t, root, "bodies")); n != 1 {
		t.Fatalf("bodies=%d, want 1", n)
	}
	stored, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p1)))
	if err != nil {
		t.Fatal(err)
	}
	var decoded ruleSnapshotManifest
	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BodyFile != m1.BodyFile || decoded.BodyBytes != len("<p>sanitized</p>") {
		t.Fatalf("bad manifest: %+v", decoded)
	}
	m3, _ := snapshotStoreWrite(t, root, 3, "different")
	if m3.BodyFile == m1.BodyFile || len(snapshotStoreEntries(t, root, "bodies")) != 2 {
		t.Fatal("different sanitized bytes shared a body")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(m1.BodyFile)), 0o644); err != nil {
			t.Fatal(err)
		}
		snapshotStoreWrite(t, root, 4, "<p>sanitized</p>")
		for _, rel := range []string{p1, m1.BodyFile} {
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("private file permissions: %s: %v %v", rel, info, err)
			}
		}
		for _, rel := range []string{ruleSnapshotDir, ruleSnapshotDir + "/observations", ruleSnapshotDir + "/bodies"} {
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("private directory permissions: %s: %v %v", rel, info, err)
			}
		}
	}
}

func TestRuleSnapshotStoreEmptyAndOmittedBody(t *testing.T) {
	root := t.TempDir()
	m, _ := snapshotStoreWrite(t, root, 1, "")
	emptyHash := sha256.Sum256(nil)
	if m.BodySHA256 != hex.EncodeToString(emptyHash[:]) || m.BodyBytes != 0 || m.BodyFile == "" {
		t.Fatalf("empty body is valid: %+v", m)
	}
	omitted := snapshotStoreManifest(2)
	omitted.BodyOmitted = "oversize"
	m, _, err := writeRuleSnapshot(root, omitted, []byte("must not be stored"))
	if err != nil || m.BodyFile != "" || m.BodySHA256 != "" {
		t.Fatalf("omitted body was not omitted: %+v %v", m, err)
	}
	if len(snapshotStoreEntries(t, root, "bodies")) != 1 {
		t.Fatal("omitted body was written")
	}
}

func TestRuleSnapshotStoreDoesNotOverwriteObservation(t *testing.T) {
	root := t.TempDir()
	_, rel := snapshotStoreWrite(t, root, 1, "first")
	before, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	_, failedRel, err := writeRuleSnapshot(root, snapshotStoreManifest(1), []byte("second"))
	if !errors.Is(err, fs.ErrExist) || failedRel != "" {
		t.Fatalf("collision error=%v, path=%q", err, failedRel)
	}
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing observation overwritten: %v", err)
	}
	if len(snapshotStoreEntries(t, root, "bodies")) != 1 {
		t.Fatal("collision wrote an unused body")
	}
}

func TestRuleSnapshotStoreRejectsCorruptExistingBody(t *testing.T) {
	for _, corrupt := range []string{"CORRUPT", "short"} {
		t.Run(corrupt, func(t *testing.T) {
			root := t.TempDir()
			m, _ := snapshotStoreWrite(t, root, 1, "correct")
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(m.BodyFile)), []byte(corrupt), 0o600); err != nil {
				t.Fatal(err)
			}
			_, rel, err := writeRuleSnapshot(root, snapshotStoreManifest(2), []byte("correct"))
			if err == nil || rel != "" || !strings.Contains(err.Error(), "corrupt") {
				t.Fatalf("corruption ignored: path=%q err=%v", rel, err)
			}
			if len(snapshotStoreEntries(t, root, "observations")) != 1 {
				t.Fatal("published observation with corrupt body")
			}
		})
	}
}

func TestRuleSnapshotStoreRejectsInvalidMetadata(t *testing.T) {
	for name, change := range map[string]func(*ruleSnapshotManifest){
		"missing version":      func(m *ruleSnapshotManifest) { m.Version = 0 },
		"unknown version":      func(m *ruleSnapshotManifest) { m.Version = 2 },
		"traversal ID":         func(m *ruleSnapshotManifest) { m.EvidenceID = "../../outside" },
		"uppercase ID":         func(m *ruleSnapshotManifest) { m.EvidenceID = strings.Repeat("A", 32) },
		"bad original hash":    func(m *ruleSnapshotManifest) { m.OriginalSHA256 = "../outside" },
		"bad body hash":        func(m *ruleSnapshotManifest) { m.BodySHA256 = strings.Repeat("A", 64) },
		"mismatched body hash": func(m *ruleSnapshotManifest) { m.BodySHA256 = strings.Repeat("a", 64) },
		"traversal body path": func(m *ruleSnapshotManifest) {
			m.BodySHA256 = strings.Repeat("a", 64)
			m.BodyFile = "../outside.body"
		},
		"omitted body reference": func(m *ruleSnapshotManifest) {
			m.BodyOmitted = "oversize"
			m.BodySHA256 = strings.Repeat("a", 64)
			m.BodyFile = ruleSnapshotBodyPath(m.BodySHA256)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			m := snapshotStoreManifest(1)
			change(&m)
			if _, rel, err := writeRuleSnapshot(root, m, []byte("safe")); err == nil || rel != "" {
				t.Fatalf("invalid metadata accepted: path=%q err=%v", rel, err)
			}
			snapshotStoreExists(t, root, ruleSnapshotDir, false)
		})
	}
}

func TestRuleSnapshotStoreFailedWriteHasNoManifest(t *testing.T) {
	t.Run("manifest size limit", func(t *testing.T) {
		root := t.TempDir()
		m := snapshotStoreManifest(1)
		m.Site = strings.Repeat("s", ruleSnapshotMaxManifestBytes)
		if _, rel, err := writeRuleSnapshot(root, m, []byte("body")); err == nil || rel != "" {
			t.Fatalf("oversize manifest accepted: path=%q err=%v", rel, err)
		}
		snapshotStoreExists(t, root, ruleSnapshotDir, false)
	})
	t.Run("body destination blocked", func(t *testing.T) {
		root := t.TempDir()
		hash := sha256.Sum256([]byte("body"))
		bodyPath := ruleSnapshotBodyPath(hex.EncodeToString(hash[:]))
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(bodyPath)), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, rel, err := writeRuleSnapshot(root, snapshotStoreManifest(1), []byte("body")); err == nil || rel != "" {
			t.Fatalf("blocked body accepted: path=%q err=%v", rel, err)
		}
		if len(snapshotStoreEntries(t, root, "observations")) != 0 {
			t.Fatal("failed write published a manifest")
		}
	})
}

func TestRuleSnapshotStoreConcurrentWritersAndCleanup(t *testing.T) {
	root := t.TempDir()
	// Bootstrap directories before the read-only observer starts.
	snapshotStoreWrite(t, root, 1, "body-0")
	const writers = 24
	errs := make(chan error, writers+1)
	stop := make(chan struct{})
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			entries, err := os.ReadDir(filepath.Join(root, ruleSnapshotDir, "observations"))
			if err != nil {
				errs <- err
				return
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(root, ruleSnapshotDir, "observations", entry.Name()))
				var m ruleSnapshotManifest
				if err == nil {
					err = json.Unmarshal(data, &m)
				}
				if err == nil {
					data, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(m.BodyFile)))
				}
				if err == nil {
					h := sha256.Sum256(data)
					if hex.EncodeToString(h[:]) != m.BodySHA256 {
						err = errors.New("published body is incomplete")
					}
				}
				if err != nil {
					errs <- fmt.Errorf("partially published observation: %w", err)
					return
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := writeRuleSnapshot(root, snapshotStoreManifest(i+2), []byte(fmt.Sprintf("body-%d", i%3)))
			if err != nil {
				errs <- err
			}
			cleanupRuleSnapshots(context.Background(), root, time.Hour)
		}(i)
	}
	wg.Wait()
	close(stop)
	<-observerDone
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(snapshotStoreEntries(t, root, "observations")) != writers+1 || len(snapshotStoreEntries(t, root, "bodies")) != 3 {
		t.Fatal("concurrent writes/GC lost an observation or duplicated a body")
	}
}

func TestRuleSnapshotCleanupRetainsReferencedOldBodyAcrossObservations(t *testing.T) {
	root := t.TempDir()
	shared, expired := snapshotStoreWrite(t, root, 1, "shared")
	liveInput := snapshotStoreManifest(2)
	liveInput.Site, liveInput.Stage = "another-site", "another-stage"
	_, live, err := writeRuleSnapshot(root, liveInput, []byte("shared"))
	if err != nil {
		t.Fatal(err)
	}
	orphan, expiredOrphan := snapshotStoreWrite(t, root, 3, "orphan")
	fresh, freshPath := snapshotStoreWrite(t, root, 4, "fresh")
	oldTemps := []string{
		ruleSnapshotDir + "/observations/.snapshot-" + strings.Repeat("a", 32) + ".tmp",
		ruleSnapshotDir + "/bodies/.snapshot-" + strings.Repeat("b", 32) + ".tmp",
	}
	freshTemp := ruleSnapshotDir + "/bodies/.snapshot-" + strings.Repeat("c", 32) + ".tmp"
	for _, rel := range append(append([]string(nil), oldTemps...), freshTemp) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshotStoreAge(t, root, expired, expiredOrphan, shared.BodyFile, orphan.BodyFile)
	snapshotStoreAge(t, root, oldTemps...)
	if n := cleanupRuleSnapshots(context.Background(), root, 24*time.Hour); n != 5 {
		t.Fatalf("removed=%d, want 2 observations + 1 orphan + 2 temp files", n)
	}
	for _, rel := range []string{shared.BodyFile, live, fresh.BodyFile, freshPath, freshTemp} {
		snapshotStoreExists(t, root, rel, true)
	}
	for _, rel := range append([]string{expired, expiredOrphan, orphan.BodyFile}, oldTemps...) {
		snapshotStoreExists(t, root, rel, false)
	}
	snapshotStoreAge(t, root, live)
	if n := cleanupRuleSnapshots(context.Background(), root, 24*time.Hour); n != 2 {
		t.Fatalf("last observation expired: removed=%d, want observation + shared body", n)
	}
	snapshotStoreExists(t, root, shared.BodyFile, false)
}

func TestRuleSnapshotCleanupMalformedRecentManifestPreservesBodies(t *testing.T) {
	for _, kind := range []string{"invalid JSON", "invalid ID", "invalid body path", "unknown version", "oversized", "unreadable directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			m, rel := snapshotStoreWrite(t, root, 1, "preserve me")
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
			snapshotStoreAge(t, root, m.BodyFile)
			badPath := filepath.Join(root, ruleSnapshotDir, "observations", fmt.Sprintf("%032x.json", 2))
			data := []byte("{broken")
			switch kind {
			case "invalid ID":
				m.EvidenceID = "../outside"
				data, _ = json.Marshal(m)
			case "invalid body path":
				m.EvidenceID = fmt.Sprintf("%032x", 2)
				m.BodyFile = "../outside.body"
				data, _ = json.Marshal(m)
			case "unknown version":
				// A future format can move body references into unknown fields.
				// Treating its known fields as authoritative would delete this body.
				data, _ = json.Marshal(map[string]any{
					"version": 2, "evidence_id": fmt.Sprintf("%032x", 2),
					"body_omitted": "future-format", "body_refs": []string{m.BodyFile},
				})
			case "oversized":
				data = bytes.Repeat([]byte(" "), ruleSnapshotMaxManifestBytes+1)
			case "unreadable directory":
				if err := os.Mkdir(badPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "unreadable directory" {
				if err := os.WriteFile(badPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if n := cleanupRuleSnapshots(context.Background(), root, 24*time.Hour); n != 0 {
				t.Fatalf("uncertain reference caused deletion: %d", n)
			}
			// Use the original canonical path even when the malformed manifest was changed.
			h := sha256.Sum256([]byte("preserve me"))
			snapshotStoreExists(t, root, ruleSnapshotBodyPath(hex.EncodeToString(h[:])), true)
		})
	}
}

func TestRuleSnapshotCleanupCancelled(t *testing.T) {
	root := t.TempDir()
	m, rel := snapshotStoreWrite(t, root, 1, "keep")
	snapshotStoreAge(t, root, rel, m.BodyFile)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := cleanupRuleSnapshots(ctx, root, time.Hour); n != 0 {
		t.Fatalf("cancelled cleanup removed %d files", n)
	}
	snapshotStoreExists(t, root, rel, true)
	snapshotStoreExists(t, root, m.BodyFile, true)
}

func TestRuleSnapshotStoreAndCleanupRootConfinement(t *testing.T) {
	for _, dir := range []string{ruleSnapshotDir, ruleSnapshotDir + "/observations", ruleSnapshotDir + "/bodies"} {
		t.Run(dir, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(root, filepath.FromSlash(dir))
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			sentinel := filepath.Join(outside, strings.Repeat("a", 64)+".body")
			if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(sentinel, old, old); err != nil {
				t.Fatal(err)
			}
			if _, rel, err := writeRuleSnapshot(root, snapshotStoreManifest(1), []byte("body")); err == nil || rel != "" {
				t.Fatalf("symlink escape accepted: path=%q err=%v", rel, err)
			}
			cleanupRuleSnapshots(context.Background(), root, time.Hour)
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "outside" {
				t.Fatalf("cleanup touched outside root: %q %v", data, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("store wrote outside root: %v %v", entries, err)
			}
		})
	}
}
