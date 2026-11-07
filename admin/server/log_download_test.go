package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogDownloadRejectsNonLogsAndPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.log", "private.txt", "crawler.pid", ".private.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("private-content"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "folder.log"), 0700); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
	for _, name := range []string{"private.txt", "crawler.pid", ".private.log", "folder.log", "../a.log", `..\a.log`, "/a.log", ".", "..", "a.log:secret", "a.log\x00"} {
		t.Run(name, func(t *testing.T) {
			rr := serve(m, http.MethodGet, "/api/logs/download?file="+url.QueryEscape(name), nil)
			if rr.Code != http.StatusBadRequest || strings.Contains(rr.Body.String(), "private-content") {
				t.Fatalf("unsafe log download: status=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
	if rr := serve(m, http.MethodGet, "/api/logs/download?file=gone.log", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("missing file status=%d", rr.Code)
	}
}

func TestLogDownloadRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("outside-private-content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.log")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
	for _, endpoint := range []string{"download", "tail"} {
		rr := serve(m, http.MethodGet, "/api/logs/"+endpoint+"?file=linked.log", nil)
		if rr.Code != http.StatusBadRequest || strings.Contains(rr.Body.String(), "outside-private-content") {
			t.Errorf("%s followed symlink: status=%d body=%q", endpoint, rr.Code, rr.Body.String())
		}
	}
}

func TestLogDownloadPreservesCompressedFilesAndHTTPFeatures(t *testing.T) {
	dir := t.TempDir()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("compressed log content")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sys.log", "sys.log.1", "sys.log.gz", "sys.log.1.gz"} {
		data := []byte("plain log content")
		if strings.HasSuffix(name, ".gz") {
			data = compressed.Bytes()
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: dir})
		rr := serve(m, http.MethodGet, "/api/logs/download?file="+name, nil)
		if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
			t.Fatalf("download %s: status=%d body=%q", name, rr.Code, rr.Body.String())
		}
		mux := http.NewServeMux()
		m.Register(mux)
		req := httptest.NewRequest(http.MethodGet, "/api/logs/download?file="+name, nil)
		req.Header.Set("Range", "bytes=0-4")
		rr = httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusPartialContent || !bytes.Equal(rr.Body.Bytes(), data[:5]) {
			t.Errorf("range %s: status=%d body=%q", name, rr.Code, rr.Body.String())
		}
		rr = serve(m, http.MethodHead, "/api/logs/download?file="+name, nil)
		if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
			t.Errorf("HEAD %s: status=%d body=%q", name, rr.Code, rr.Body.String())
		}
	}
}

func TestLogDownloadRequiresAuthentication(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		LogDir:      t.TempDir(),
		VerifyToken: func(r *http.Request) (string, bool) { return "", false },
	})
	for _, target := range []string{"/api/logs/download?file=a.log", "/api/logs/download"} {
		if rr := serve(m, http.MethodGet, target, nil); rr.Code != http.StatusUnauthorized {
			t.Errorf("unprotected download endpoint %s: %d", target, rr.Code)
		}
	}
}
