package tasktrace

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArchiveDownloadRejectsParentJunction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction regression")
	}
	root, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.WriteFile(filepath.Join(outside, "page.html"), []byte("outside archive secret"), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path "+quote(link)+" -Target "+quote(outside)+" | Out-Null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test junction: %v %s", err, out)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil {
			t.Error(err)
		}
	})
	m := New(Options{ArchiveDir: root})
	rr := serve(m, http.MethodGet, "/api/task/page?file=linked/page.html", nil)
	if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), "outside archive secret") {
		t.Fatalf("archive escape through parent junction: status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestArchiveDownloadConfinesOpenedFile(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	const secret = "outside archive secret"
	if err := os.WriteFile(filepath.Join(outside, "page.html"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	m := New(Options{ArchiveDir: root})
	for _, tc := range []struct {
		name, target, requested string
	}{
		{"file-link", filepath.Join(outside, "page.html"), "file-link"},
		{"parent-link", outside, "parent-link/page.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Symlink(tc.target, filepath.Join(root, tc.name)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			rr := serve(m, http.MethodGet, "/api/task/page?file="+url.QueryEscape(tc.requested), nil)
			if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), secret) {
				t.Fatalf("archive escape: status=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
	t.Run("regular-file", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		const body = "<html>ordinary archived page</html>"
		if err := os.WriteFile(filepath.Join(root, "nested", "page.html"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		rr := serve(m, http.MethodGet, "/api/task/page?file=nested/page.html", nil)
		if rr.Code != http.StatusOK || rr.Body.String() != body || rr.Header().Get("Content-Disposition") != `attachment; filename="page.html"` {
			t.Fatalf("normal archive download: status=%d headers=%v body=%q", rr.Code, rr.Header(), rr.Body.String())
		}
	})
}

func TestArchiveDownloadSupportsRelativeRootAndRejectsDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("archive", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	const body = "relative archive page"
	if err := os.WriteFile(filepath.Join("archive", "nested", "page.html"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"archive", "." + string(filepath.Separator) + "archive"} {
		m := New(Options{ArchiveDir: root})
		rr := serve(m, http.MethodGet, "/api/task/page?file=nested/page.html", nil)
		if rr.Code != http.StatusOK || rr.Body.String() != body {
			t.Fatalf("relative root %q: status=%d body=%q", root, rr.Code, rr.Body.String())
		}
		rr = serve(m, http.MethodGet, "/api/task/page?file=nested", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("directory under %q accepted: status=%d", root, rr.Code)
		}
	}
}

func serve(h *Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	w := httptest.NewRecorder()
	h.Page(w, r)
	return w
}
