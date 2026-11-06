package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

const ruleSnapshotMaxManifestBytes = 256 << 10

// All Engines in this process share publication/GC exclusion. This is not a
// cross-process lock: independently running crawlers need separate archives.
var ruleSnapshotStoreMu sync.Mutex

func ruleSnapshotHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ruleSnapshotBodyPath(hash string) string {
	return ruleSnapshotDir + "/bodies/" + hash + ".body"
}

func validateRuleSnapshotMetadata(m ruleSnapshotManifest) error {
	if m.Version != 1 {
		return errors.New("unsupported rule snapshot manifest version")
	}
	if !ruleSnapshotHex(m.EvidenceID, 32) {
		return errors.New("invalid rule snapshot evidence ID")
	}
	if m.OriginalSHA256 != "" && !ruleSnapshotHex(m.OriginalSHA256, 64) {
		return errors.New("invalid rule snapshot original hash")
	}
	if m.BodySHA256 != "" && !ruleSnapshotHex(m.BodySHA256, 64) {
		return errors.New("invalid rule snapshot body hash")
	}
	if m.BodyFile != "" && (m.BodySHA256 == "" || m.BodyFile != ruleSnapshotBodyPath(m.BodySHA256)) {
		return errors.New("invalid rule snapshot body path")
	}
	if m.BodyOmitted != "" && (m.BodyFile != "" || m.BodySHA256 != "") {
		return errors.New("omitted rule snapshot body has a reference")
	}
	return nil
}

// writeRuleSnapshot accepts only the already-sanitized body. The immutable
// observation is published last, so every successful manifest has its body.
func writeRuleSnapshot(root string, manifest ruleSnapshotManifest, body []byte) (ruleSnapshotManifest, string, error) {
	if err := validateRuleSnapshotMetadata(manifest); err != nil {
		return ruleSnapshotManifest{}, "", err
	}
	if root == "" {
		return ruleSnapshotManifest{}, "", errors.New("empty rule snapshot root")
	}
	if manifest.BodyOmitted == "" {
		hash := sha256.Sum256(body)
		hexdigest := hex.EncodeToString(hash[:])
		if manifest.BodySHA256 != "" && manifest.BodySHA256 != hexdigest {
			return ruleSnapshotManifest{}, "", errors.New("rule snapshot body hash does not match sanitized bytes")
		}
		manifest.BodySHA256 = hexdigest
		manifest.BodyFile = ruleSnapshotBodyPath(hexdigest)
		manifest.BodyBytes = len(body)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return ruleSnapshotManifest{}, "", fmt.Errorf("encode rule snapshot: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > ruleSnapshotMaxManifestBytes {
		return ruleSnapshotManifest{}, "", errors.New("rule snapshot manifest exceeds 256 KiB")
	}

	ruleSnapshotStoreMu.Lock()
	defer ruleSnapshotStoreMu.Unlock()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ruleSnapshotManifest{}, "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return ruleSnapshotManifest{}, "", err
	}
	defer r.Close()
	for _, dir := range []string{ruleSnapshotDir, ruleSnapshotDir + "/observations", ruleSnapshotDir + "/bodies"} {
		if err := makeRuleSnapshotDir(r, dir); err != nil {
			return ruleSnapshotManifest{}, "", err
		}
	}
	rel := ruleSnapshotDir + "/observations/" + manifest.EvidenceID + ".json"
	if err := ruleSnapshotRequireAbsent(r, rel); err != nil {
		return ruleSnapshotManifest{}, "", err
	}
	if manifest.BodyOmitted == "" {
		info, err := r.Lstat(manifest.BodyFile)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return ruleSnapshotManifest{}, "", errors.New("rule snapshot body is not a regular file")
			}
			if err := verifyRuleSnapshotBody(r, manifest.BodyFile, manifest.BodySHA256, len(body)); err != nil {
				return ruleSnapshotManifest{}, "", err
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := publishRuleSnapshotFile(r, manifest.BodyFile, body); err != nil {
				return ruleSnapshotManifest{}, "", err
			}
		default:
			return ruleSnapshotManifest{}, "", err
		}
	}
	if err := publishRuleSnapshotFile(r, rel, encoded); err != nil {
		return ruleSnapshotManifest{}, "", err
	}
	return manifest, rel, nil
}

func makeRuleSnapshotDir(r *os.Root, name string) error {
	if err := r.MkdirAll(name, 0o700); err != nil {
		return err
	}
	info, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rule snapshot directory is not a real directory: %s", name)
	}
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0o700)
}

func ruleSnapshotRequireAbsent(r *os.Root, name string) error {
	_, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("rule snapshot already exists: %s: %w", name, fs.ErrExist)
	}
	return err
}

func verifyRuleSnapshotBody(r *os.Root, name, hash string, size int) error {
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(size) {
		return errors.New("existing rule snapshot body is corrupt")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, int64(size)+1))
	if err != nil {
		return err
	}
	if n != int64(size) || hex.EncodeToString(h.Sum(nil)) != hash {
		return errors.New("existing rule snapshot body is corrupt")
	}
	// Tighten permissions when an archive was restored with a looser mode.
	return f.Chmod(0o600)
}

func publishRuleSnapshotFile(r *os.Root, name string, data []byte) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	dir := name[:strings.LastIndexByte(name, '/')]
	tmp := dir + "/.snapshot-" + hex.EncodeToString(id[:]) + ".tmp"
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ruleSnapshotRequireAbsent(r, name); err != nil {
		return err
	}
	return r.Rename(tmp, name)
}

func ruleSnapshotTemp(name string) bool {
	return strings.HasPrefix(name, ".snapshot-") && strings.HasSuffix(name, ".tmp") &&
		ruleSnapshotHex(strings.TrimSuffix(strings.TrimPrefix(name, ".snapshot-"), ".tmp"), 32)
}

// cleanupRuleSnapshots performs reference-aware GC under the same lock as
// publication. Unknown/unreadable surviving observations disable body deletion
// for this pass; uncertainty must not turn a useful observation into a dangling
// reference. Only known regular files are removed, never directory trees.
func cleanupRuleSnapshots(ctx context.Context, root string, retention time.Duration) int {
	if ctx.Err() != nil || root == "" || retention <= 0 {
		return 0
	}
	ruleSnapshotStoreMu.Lock()
	defer ruleSnapshotStoreMu.Unlock()
	if ctx.Err() != nil {
		return 0
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return 0
	}
	defer r.Close()
	info, err := r.Lstat(ruleSnapshotDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0
	}
	cutoff := time.Now().Add(-retention)
	refs := make(map[string]bool)
	removed := 0
	observations, err := readRuleSnapshotDir(r, ruleSnapshotDir+"/observations")
	// A missing observations directory means an interrupted first publication;
	// every body is unreferenced. Other failures may conceal live observations.
	bodyGC := err == nil || errors.Is(err, fs.ErrNotExist)
	for _, entry := range observations {
		if ctx.Err() != nil {
			return removed
		}
		name := ruleSnapshotDir + "/observations/" + entry.Name()
		info, err := entry.Info()
		if err != nil {
			bodyGC = false
			continue
		}
		isManifest := strings.HasSuffix(entry.Name(), ".json")
		if !info.Mode().IsRegular() {
			if isManifest {
				bodyGC = false
			}
			continue
		}
		if !isManifest && !ruleSnapshotTemp(entry.Name()) {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := r.Remove(name); err == nil {
				removed++
				continue
			}
		}
		if !isManifest {
			continue
		}
		m, err := readRuleSnapshotManifest(r, name)
		if err != nil || m.EvidenceID+".json" != entry.Name() {
			bodyGC = false
			continue
		}
		if m.BodyFile != "" {
			refs[m.BodyFile] = true
		}
	}
	bodies, err := readRuleSnapshotDir(r, ruleSnapshotDir+"/bodies")
	if err != nil {
		return removed
	}
	for _, entry := range bodies {
		if ctx.Err() != nil {
			return removed
		}
		name := ruleSnapshotDir + "/bodies/" + entry.Name()
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		isBody := strings.HasSuffix(entry.Name(), ".body") &&
			ruleSnapshotHex(strings.TrimSuffix(entry.Name(), ".body"), 64)
		if !ruleSnapshotTemp(entry.Name()) && !(bodyGC && isBody && !refs[name]) {
			continue
		}
		if err := r.Remove(name); err == nil {
			removed++
		}
	}
	return removed
}

func readRuleSnapshotDir(r *os.Root, name string) ([]os.DirEntry, error) {
	info, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("rule snapshot directory is not a real directory")
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func readRuleSnapshotManifest(r *os.Root, name string) (ruleSnapshotManifest, error) {
	f, err := r.Open(name)
	if err != nil {
		return ruleSnapshotManifest{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, ruleSnapshotMaxManifestBytes+1))
	if err != nil {
		return ruleSnapshotManifest{}, err
	}
	if len(data) > ruleSnapshotMaxManifestBytes {
		return ruleSnapshotManifest{}, errors.New("rule snapshot manifest exceeds 256 KiB")
	}
	var m ruleSnapshotManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return ruleSnapshotManifest{}, err
	}
	if err := validateRuleSnapshotMetadata(m); err != nil {
		return ruleSnapshotManifest{}, err
	}
	if m.BodyOmitted == "" && (m.BodySHA256 == "" || m.BodyFile == "") {
		return ruleSnapshotManifest{}, errors.New("rule snapshot manifest has no body reference or omission reason")
	}
	return m, nil
}
