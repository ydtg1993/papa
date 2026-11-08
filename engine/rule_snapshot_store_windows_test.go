//go:build windows

package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"golang.org/x/sys/windows"
)

func TestRuleSnapshotStoreWindowsRenameSharing(t *testing.T) {
	for _, target := range []string{"manifest", "body"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			input := snapshotStoreManifest(1)
			input.SourceURL, input.StatusCode = "https://example.test/detail", 200
			body := []byte("<p>complete snapshot</p>")
			m, manifest, err := writeRuleSnapshot(dir, input, body)
			if err != nil {
				t.Fatal(err)
			}
			r, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			name := manifest
			if target == "body" {
				name = m.BodyFile
			}
			original, err := r.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			tmp := name + ".tmp"
			if err := r.Rename(name, tmp); err != nil {
				t.Fatal(err)
			}
			path, err := windows.UTF16PtrFromString(filepath.Join(dir, filepath.FromSlash(tmp)))
			if err != nil {
				t.Fatal(err)
			}
			// Go's Windows Root.Rename opens the source with DELETE access,
			// renames it, then closes the handle. Hold an equivalent handle
			// across a real rename to pin that otherwise tiny visibility window.
			h, err := windows.CreateFile(path, windows.DELETE|windows.SYNCHRONIZE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if h != windows.InvalidHandle {
					windows.CloseHandle(h)
				}
			}()
			if err := r.Rename(tmp, name); err != nil {
				t.Fatal(err)
			}
			abs := filepath.Join(dir, filepath.FromSlash(name))
			if _, err := os.ReadFile(abs); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("ordinary reader: got %v, want ERROR_SHARING_VIOLATION", err)
			} else {
				t.Logf("reproduced rename window: %v", err)
			}
			if err := snapshotStoreObserve(r, manifest); err != nil {
				t.Errorf("observer rejected a complete published snapshot: %v", err)
			}
			// Exercise the production consumer too, without retrying or locking.
			sample, err := rulestore.ReadSnapshot(context.Background(), dir, m.EvidenceID)
			if err != nil || !bytes.Equal(sample.Body, body) {
				t.Errorf("production reader: body=%q err=%v", sample.Body, err)
			}
			// Sharing DELETE must not turn a genuinely incomplete file into a
			// successful observation. Keep the same handle open while corrupting
			// the manifest/body, then restore it and check recovery without retries.
			corrupt := []byte(`{"version":`)
			if target == "body" {
				corrupt = bytes.Repeat([]byte("x"), len(original))
			}
			if err := r.WriteFile(name, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := snapshotStoreObserve(r, manifest); err == nil {
				t.Error("observer accepted corrupt snapshot during rename window")
			}
			if _, err := rulestore.ReadSnapshot(context.Background(), dir, m.EvidenceID); err == nil {
				t.Error("production reader accepted corrupt snapshot during rename window")
			}
			if err := r.WriteFile(name, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := snapshotStoreObserve(r, manifest); err != nil {
				t.Errorf("restored observation: %v", err)
			}
			sample, err = rulestore.ReadSnapshot(context.Background(), dir, m.EvidenceID)
			if err != nil || !bytes.Equal(sample.Body, body) {
				t.Errorf("restored production read: body=%q err=%v", sample.Body, err)
			}
			if err := windows.CloseHandle(h); err != nil {
				t.Fatal(err)
			}
			h = windows.InvalidHandle
			if got, err := os.ReadFile(abs); err != nil || !bytes.Equal(got, original) {
				t.Fatalf("ordinary reader after handle close: bytes match=%v err=%v", bytes.Equal(got, original), err)
			}
		})
	}
}
