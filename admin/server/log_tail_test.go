package server

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadLogTail(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		lines             int
	}{
		{"empty", "", "", 0}, {"single", "hello", "hello", 1}, {"blank", "\n", "\n", 1},
		{"crlf", "一\r\n二\r\n", "一\n二\n", 2}, {"no newline", "一\n二", "一\n二", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readLogTail(strings.NewReader(tc.input), int64(len(tc.input)))
			if err != nil || got.Text != tc.want || got.Lines != tc.lines || got.Truncated {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	for _, end := range []bool{false, true} {
		lines := []string{}
		for i := 1; i <= 300; i++ {
			lines = append(lines, fmt.Sprintf("line %d", i))
		}
		input := strings.Join(lines, "\n")
		want := strings.Join(lines[100:], "\n")
		if end {
			input += "\n"
			want += "\n"
		}
		got, err := readLogTail(strings.NewReader(input), int64(len(input)))
		if err != nil || got.Text != want || got.Lines != 200 || got.Truncated {
			t.Fatalf("tail mismatch: %+v %v", got, err)
		}
	}
}

type boundedReader struct {
	*strings.Reader
	bytes int
}

func (r *boundedReader) ReadAt(p []byte, off int64) (int, error) {
	n, e := r.Reader.ReadAt(p, off)
	r.bytes += n
	return n, e
}
func TestLogTailByteLimitAndUTF8(t *testing.T) {
	input := strings.Repeat("中", logTailBytes)
	r := &boundedReader{Reader: strings.NewReader(input)}
	got, err := readLogTail(r, int64(len(input)))
	if err != nil || !got.Truncated || !utf8.ValidString(got.Text) || r.bytes > logTailBytes || len(got.Text) > logTailBytes {
		t.Fatalf("limit failure bytes=%d len=%d truncated=%v err=%v", r.bytes, len(got.Text), got.Truncated, err)
	}
}
func TestLogTailEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("<script>alert(1)</script>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{LogDir: dir})
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
	js, err := os.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.textContent = '查看'", "viewLog(f.name)", "/api/logs/tail?file=", "content.textContent = data.text", "label:'刷新'", "downloadFile('/api/logs/download')"} {
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
