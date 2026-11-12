package server

import (
	"archive/zip"
	"bytes"
	"github.com/ydtg1993/papa/v4/admin/logs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogTailEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("<script>alert(1)</script>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
	rr := serve(m, http.MethodGet, "/api/logs/tail?file=a.log", nil)
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if decodeJSON(t, rr)["lines"] != float64(1) {
		t.Fatal("missing line count")
	}
	for _, name := range []string{"../a.log", `..\a.log`, "/a.log", ".", ""} {
		rr = serve(m, http.MethodGet, "/api/logs/tail?file="+url.QueryEscape(name), nil)
		if rr.Code != 400 {
			t.Fatalf("accepted path %q: %d", name, rr.Code)
		}
	}
	rr = serve(m, http.MethodGet, "/api/logs/tail?file=gone.log", nil)
	if rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.log.gz"), []byte("compressed"), 0600); err != nil {
		t.Fatal(err)
	}
	rr = serve(m, http.MethodGet, "/api/logs/tail?file=old.log.gz", nil)
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	outside := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.log")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	} else {
		rr = serve(m, http.MethodGet, "/api/logs/tail?file=link.log", nil)
		if rr.Code != 400 {
			t.Fatal("symlink allowed", rr.Code)
		}
	}
}

func TestLogViewerUIContract(t *testing.T) {
	js, err := staticFS.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.textContent = '查看'", "viewLog(f.name)", "/api/logs/tail?file=", "content.textContent = data.text", "label:'刷新'", "downloadFile('/api/logs/download',"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(string(js), "downloadLog(f.name)") {
		t.Fatal("single download action remains")
	}
}

func TestLogTailRequiresAuthentication(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{LogDir: t.TempDir(), VerifyToken: func(r *http.Request) (string, bool) { return "", false }})
	rr := serve(m, http.MethodGet, "/api/logs/tail?file=a.log", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected tail endpoint: %d", rr.Code)
	}
}
func TestLogListAndArchiveExcludeNonLogs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sys.log", "mediawave-webhook.log", "sys-2026-11-05.log.gz", "sys.log.1", ".gitkeep", "crawler.pid", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
	rr := serve(m, http.MethodGet, "/api/logs", nil)
	files := decodeJSON(t, rr)["files"].([]any)
	if len(files) != 4 {
		t.Fatalf("unexpected list: %v", files)
	}
	rr = serve(m, http.MethodGet, "/api/logs/download", nil)
	archive, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 4 {
		t.Fatalf("archive has %d entries", len(archive.File))
	}
	for _, f := range archive.File {
		if !logs.IsFileName(f.Name) {
			t.Fatalf("unexpected archive entry %s", f.Name)
		}
	}
}

func TestLogListUsesCompactGrid(t *testing.T) {
	css, err := staticFS.ReadFile("static/mo.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "grid-template-columns:repeat(auto-fill,minmax(min(100%,260px),1fr))") {
		t.Fatal("log list must use responsive columns")
	}
}
