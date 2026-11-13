package filedown

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResumeResourceBoundsAndLegacy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ResumeStateDir = t.TempDir()
	d := NewDownloader(cfg).WithResources(middleware.DownloadLimits{ResumeBytes: 1024, Chunks: 2}, nil)
	la := &labor{url: "http://example.com/f", filename: "f"}
	old := &ResumeState{TotalSize: 2, ChunkSize: 1, Completed: []int64{0, 1}}
	data, _ := json.Marshal(old)
	path := d.getStateFilePath(la)
	os.WriteFile(path, data, 0600)
	s, e := d.loadResumeState(la)
	if e != nil || len(s.Completed) != 2 {
		t.Fatal(s, e)
	}
	if e = d.saveResumeState(la, s); e != nil {
		t.Fatal(e)
	}
	s, e = d.loadResumeState(la)
	if e != nil || len(s.Completed) != 2 {
		t.Fatal(s, e)
	}
	os.WriteFile(path, make([]byte, 1025), 0600)
	if _, e = d.loadResumeState(la); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
	old.Completed = append(old.Completed, 2)
	data, _ = json.Marshal(old)
	os.WriteFile(path, data, 0600)
	if _, e = d.loadResumeState(la); !errors.Is(e, middleware.ErrResourceLimit) {
		t.Fatal(e)
	}
}
func TestDirectResourceLimitPreservesOutput(t *testing.T) {
	for _, n := range []int{8, 9} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.(http.Flusher).Flush(); w.Write(make([]byte, n)) }))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.Network = egress.Config{PrivateOrigins: []string{server.URL}}
			cfg.OutputDir = t.TempDir()
			cfg.ResumeStateDir = t.TempDir()
			d := NewDownloader(cfg).WithResources(middleware.DownloadLimits{OutputBytes: 8}, nil)
			out := filepath.Join(cfg.OutputDir, "f")
			os.WriteFile(out, []byte("old"), 0600)
			res := d.Download(context.Background(), server.URL, "", "f", &DownloadOptions{Direct: true})
			b, _ := os.ReadFile(out)
			if n == 8 && (res.Error != nil || len(b) != 8) {
				t.Fatal(res.Error, len(b))
			}
			if n == 9 && (!errors.Is(res.Error, middleware.ErrResourceLimit) || string(b) != "old") {
				t.Fatal(res.Error, string(b))
			}
		})
	}
}
func TestWithPreservesResourceAdmission(t *testing.T) {
	b := middleware.NewDownloadBudget(8, 10)
	d := NewDownloader(nil).WithResources(middleware.DownloadLimits{Chunks: 2}, b).With(Overlay{})
	if d.budget != b || d.limits.Chunks != 2 {
		t.Fatal("lost budget")
	}
}
