package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-rod/rod/lib/launcher"
)

// ResolveTempDir returns the absolute profile root without modifying the filesystem.
// An empty setting preserves Rod's default, including testing-mode overrides.
func ResolveTempDir(configured string) (string, error) {
	if configured == "" {
		configured = launcher.DefaultUserDataDirPrefix
	}
	if strings.TrimSpace(configured) == "" {
		return "", fmt.Errorf("browser.temp_dir cannot be whitespace")
	}
	root, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("browser.temp_dir: %w", err)
	}
	return filepath.Clean(root), nil
}

// PrepareTempDir checks real file writes, not just directory creation. Never falls back.
func PrepareTempDir(configured string) (string, error) {
	root, err := ResolveTempDir(configured)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("browser.temp_dir %q: %w", root, err)
	}
	probe, err := os.CreateTemp(root, ".write-check-")
	if err != nil {
		return "", fmt.Errorf("browser.temp_dir %q: %w", root, err)
	}
	defer os.Remove(probe.Name())
	_, writeErr := probe.Write([]byte("rod"))
	syncErr := probe.Sync()
	closeErr := probe.Close()
	for _, e := range []error{writeErr, syncErr, closeErr} {
		if e != nil {
			return "", fmt.Errorf("browser.temp_dir %q is not writable: %w", root, e)
		}
	}
	return root, nil
}

// TempDir is the immutable root actually used by this pool.
func (p *Pool) TempDir() string { return p.cfg.TempDir }

// NewTempProfile isolates an instance and routes Chromium's own temporary files
// to the same root, without changing the crawler process environment.
func NewTempProfile(l *launcher.Launcher, root, prefix string) (string, error) {
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", fmt.Errorf("browser profile in %q: %w", root, err)
	}
	l.UserDataDir(dir)
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMPDIR=") {
			env = append(env, entry)
		}
	}
	l.Env(append(env, "TMPDIR="+root)...)
	return dir, nil
}
