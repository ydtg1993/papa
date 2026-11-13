package reportrunner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionCursorClosedAtEOFAndWriterExit(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{Config: Config{Enabled: true, ReportDir: dir}})
	if err := r.prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.pruning.dir != nil {
		t.Fatal("empty-directory EOF retained cursor")
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.pruning.dir == nil {
		t.Fatal("fixture did not retain a live directory cursor")
	}
	opened := r.pruning.dir
	r.Start()
	closeRunner(t, r)
	if r.pruning.dir != nil {
		t.Fatal("writer exit retained cursor")
	}
	if _, err := opened.ReadDir(1); err == nil {
		t.Fatal("writer dropped cursor without closing its handle")
	}
}
