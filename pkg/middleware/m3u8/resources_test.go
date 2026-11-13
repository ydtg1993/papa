package m3u8

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaylistResourceBoundaries(t *testing.T) {
	d := NewDownloader(nil).WithResources(middleware.DownloadLimits{Segments: 2, PlaylistBytes: 20}, nil)
	if _, s, _, e := d.parsePlaylistEnhancedWithIndex("a.ts\nb.ts\n", "https://x/"); e != nil || len(s) != 2 {
		t.Fatal(s, e)
	}
	if _, _, _, e := d.parsePlaylistEnhancedWithIndex("a.ts\nb.ts\nc.ts\n", "https://x/"); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
	if _, _, _, e := d.parsePlaylistEnhancedWithIndex(strings.Repeat("#", 21), "https://x/"); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
}
func TestMediaResourceBoundaries(t *testing.T) {
	for _, n := range []int{8, 9} {
		for _, declared := range []bool{false, true} {
			t.Run(strings.Repeat("x", n)+map[bool]string{true: "declared", false: "chunked"}[declared], func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !declared {
						w.(http.Flusher).Flush()
					}
					w.Write(make([]byte, n))
				}))
				defer server.Close()
				cfg := DefaultConfig()
				cfg.MaxRetries = 0
				cfg.Network.PrivateOrigins = []string{server.URL}
				d := NewDownloader(cfg).WithResources(middleware.DownloadLimits{MediaBytes: 8}, nil)
				dest := filepath.Join(t.TempDir(), "segment")
				e := d.downloadSegmentToFile(context.Background(), &SegmentInfo{URL: server.URL}, nil, nil, dest, nil)
				if n == 8 {
					if e != nil {
						t.Fatal(e)
					}
					b, _ := os.ReadFile(dest)
					if len(b) != 8 {
						t.Fatal(len(b))
					}
				} else {
					if !errors.Is(e, middleware.ErrResourceLimit) {
						t.Fatal(e)
					}
					if _, e = os.Stat(dest); !os.IsNotExist(e) {
						t.Fatal("published oversize segment")
					}
					if _, e = os.Stat(dest + ".tmp"); !os.IsNotExist(e) {
						t.Fatal("retained partial")
					}
				}
			})
		}
	}
}
func TestResumeLegacyCompactAndLimits(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ResumeStateDir = t.TempDir()
	d := NewDownloader(cfg).WithResources(middleware.DownloadLimits{Segments: 2, ResumeBytes: 1024}, nil)
	la := &labor{source: "https://x/list", filename: "out"}
	old := ResumeState{TotalSegments: 2, Completed: []int{0, 1}, SegmentFiles: []string{"a", "b"}}
	b, _ := json.Marshal(old)
	os.WriteFile(d.getStateFilePath(la), b, 0600)
	s, e := d.loadResumeState(la)
	if e != nil || len(s.Completed) != 2 {
		t.Fatal(s, e)
	}
	if e = d.saveResumeState(la, s); e != nil {
		t.Fatal(e)
	}
	s, e = d.loadResumeState(la)
	if e != nil || len(s.Completed) != 2 || len(s.SegmentFiles) != 2 {
		t.Fatal(s, e)
	}
	old.TotalSegments = 3
	b, _ = json.Marshal(old)
	os.WriteFile(d.getStateFilePath(la), b, 0600)
	if _, e = d.loadResumeState(la); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
	os.WriteFile(d.getStateFilePath(la), make([]byte, 1025), 0600)
	if _, e = d.loadResumeState(la); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
}
func TestWithResourcesPreserved(t *testing.T) {
	b := middleware.NewDownloadBudget(8, 10)
	d := NewDownloader(nil).WithResources(middleware.DownloadLimits{Segments: 2}, b).With(Overlay{})
	if d.budget != b || d.limits.Segments != 2 {
		t.Fatal("lost limits")
	}
}

func TestAssemblyLimitAndCancellationPreserveOldOutput(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "part")
	out := filepath.Join(dir, "out")
	os.WriteFile(part, []byte("123456789"), 0600)
	os.WriteFile(out, []byte("old"), 0600)
	d := NewDownloader(nil).WithResources(middleware.DownloadLimits{OutputBytes: 8}, nil)
	if e := d.concatTSFilesContext(context.Background(), "", []string{part}, out); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "old" {
		t.Fatal("replaced old output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := d.concatTSFilesContext(ctx, "", []string{part}, out); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(out)
	if string(b) != "old" {
		t.Fatal("cancellation replaced old output")
	}
}
func TestRequestRolesLimitKeyAndInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.(http.Flusher).Flush(); w.Write(make([]byte, 17)) }))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.MaxRetries = 0
	cfg.Network.PrivateOrigins = []string{server.URL}
	d := NewDownloader(cfg).WithResources(middleware.DownloadLimits{KeyBytes: 16, InitBytes: 16}, nil)
	if _, _, e := d.prepareKey(context.Background(), &KeyInfo{URL: server.URL}, 0, nil); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
	if _, _, e := d.requestWithLimit(context.Background(), server.URL, "", nil, d.limits.InitBytes, nil, nil); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
}

func TestCompactResumeEliminatesRepeatedPaths(t *testing.T) {
	d := NewDownloader(nil)
	s := ResumeState{TotalSegments: 10000, SegmentFiles: make([]string, 10000)}
	for i := range s.SegmentFiles {
		s.Completed = append(s.Completed, i)
		s.SegmentFiles[i] = strings.Repeat("directory/", 10) + "segment.ts"
	}
	old, _ := json.Marshal(s)
	compact, e := d.encodeResume(s)
	if e != nil {
		t.Fatal(e)
	}
	if len(compact)*10 >= len(old) {
		t.Fatal(len(old), len(compact))
	}
	t.Logf("legacy=%d compact=%d", len(old), len(compact))
	var restored ResumeState
	if e = d.decodeResume(compact, &restored); e != nil || len(restored.Completed) != 10000 {
		t.Fatal(e, len(restored.Completed))
	}
}
