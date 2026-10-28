package eventlog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDiscoverFindsBackups(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	oldBackup := filepath.Join(dir, "events-2026-01-02T01-00-00.000.jsonl")
	newBackup := filepath.Join(dir, "events-2026-01-02T02-00-00.000.jsonl.gz")
	writeJSONL(t, active, testEvent("run-1", 3, testAt(20)))
	writeJSONL(t, oldBackup, testEvent("run-1", 1, testAt(0)))
	writeGzipJSONL(t, newBackup, testEvent("run-1", 2, testAt(10)))
	setModTime(t, oldBackup, testAt(60))
	setModTime(t, active, testAt(120))
	setModTime(t, newBackup, testAt(180))

	// 干扰项：前缀不匹配、时间戳非法、名字相似但实际是目录，都不该被识别成备份。
	writeJSONL(t, filepath.Join(dir, "other.jsonl"), testEvent("run-9", 1, testAt(0)))
	writeJSONL(t, filepath.Join(dir, "events-backup.jsonl"), testEvent("run-8", 1, testAt(0)))
	if err := os.Mkdir(filepath.Join(dir, "events-2026-01-02T03-00-00.000.jsonl"), 0o755); err != nil {
		t.Fatalf("mkdir fake backup dir: %v", err)
	}

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{oldBackup, active, newBackup}
	if !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}
}

func TestDiscoverMissingMainFile(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	backup := filepath.Join(dir, "events-2026-01-02T01-00-00.000.jsonl")
	writeJSONL(t, backup, testEvent("run-1", 1, testAt(0)))

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{backup}; !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}
}

func TestDiscoverActiveFileOnly(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	writeJSONL(t, active, testEvent("run-1", 1, testAt(0)))

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{active}; !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}
}

func TestDiscoverNoFiles(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "other.jsonl"), testEvent("run-1", 1, testAt(0)))

	paths, err := Discover(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Discover empty dir: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("Discover = %v, want none", paths)
	}

	missingDir := filepath.Join(dir, "no-such-dir", "events.jsonl")
	paths, err = Discover(missingDir)
	if err != nil {
		t.Fatalf("Discover missing dir: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("Discover missing dir = %v, want none", paths)
	}
}

func TestDiscoverEmptyPath(t *testing.T) {
	if _, err := Discover(""); err == nil {
		t.Fatal("Discover empty path: want error, got nil")
	}
	if _, err := Discover("   "); err == nil {
		t.Fatal("Discover blank path: want error, got nil")
	}
}

func TestDiscoverExtensionlessMainFile(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events")
	plainBackup := filepath.Join(dir, "events-2026-01-02T01-00-00.000")
	gzBackup := filepath.Join(dir, "events-2026-01-02T02-00-00.000.gz")
	writeJSONL(t, active, testEvent("run-1", 3, testAt(20)))
	writeJSONL(t, plainBackup, testEvent("run-1", 1, testAt(0)))
	writeGzipJSONL(t, gzBackup, testEvent("run-1", 2, testAt(10)))
	setModTime(t, plainBackup, testAt(60))
	setModTime(t, gzBackup, testAt(120))
	setModTime(t, active, testAt(180))

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{plainBackup, gzBackup, active}
	if !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}
}

func TestDiscoverEqualModTimeOrderedByPath(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	backup1 := filepath.Join(dir, "events-2026-01-02T01-00-00.000.jsonl")
	backup2 := filepath.Join(dir, "events-2026-01-02T02-00-00.000.jsonl")
	writeJSONL(t, active, testEvent("run-1", 3, testAt(20)))
	writeJSONL(t, backup1, testEvent("run-1", 1, testAt(0)))
	writeJSONL(t, backup2, testEvent("run-1", 2, testAt(10)))
	for _, path := range []string{active, backup1, backup2} {
		setModTime(t, path, testAt(60))
	}

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{backup1, backup2, active}
	if !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}
}
