package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestLogsListHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.log"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
	rr := serve(m, http.MethodGet, "/api/logs", nil)
	body := decodeJSON(t, rr)

	files := body["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files count = %d, want 2", len(files))
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"a.log", "b.log"} {
		if !names[n] {
			t.Errorf("missing log file %q", n)
		}
	}
}

func TestLogsDownloadHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})

	rr := serve(m, http.MethodGet, "/api/logs/download?file=a.log", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("single download status = %d", rr.Code)
	}
	if rr.Body.String() != "hello" {
		t.Errorf("single download body = %q, want %q", rr.Body.String(), "hello")
	}

	rr2 := serve(m, http.MethodGet, "/api/logs/download", nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("zip download status = %d", rr2.Code)
	}
	if ct := rr2.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip content-type = %q, want application/zip", ct)
	}
}
