package filedown

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestProxyOverlayTimeoutIsolation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = 7 * time.Second
	d := NewDownloader(cfg)
	base := d.clientForProxy("http://127.0.0.1:9")
	timeout := 1
	derived := d.With(Overlay{Timeout: &timeout}).clientForProxy("http://127.0.0.1:9")
	if derived.Timeout != time.Second {
		t.Fatalf("overlay timeout ignored: got %v want %v", derived.Timeout, timeout)
	}
	if base.Timeout != 7*time.Second {
		t.Fatal("base timeout mutated")
	}
	if derived.Transport != base.Transport {
		t.Fatal("proxy transport must be shared")
	}
}

func TestChunkSchedulingBounded(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "2000")
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		once.Do(func() { close(entered) })
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.Network.PrivateOrigins = []string{server.URL}
	cfg.ChunkSize = 1
	cfg.MaxConcurrent = 2
	cfg.MaxRetries = 0
	d := NewDownloader(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseline := runtime.NumGoroutine()
	done := make(chan *DownloadResult, 1)
	go func() { done <- d.Download(ctx, server.URL, "", "out") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no request")
	}
	time.Sleep(100 * time.Millisecond)
	growth := runtime.NumGoroutine() - baseline
	cancel()
	select {
	case res := <-done:
		if res.Error == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation stuck")
	}
	if growth > 100 {
		t.Fatal(fmt.Sprintf("unbounded goroutine growth: %d for concurrency 2", growth))
	}
}

func TestChunkRangeNearInt64Limit(t *testing.T) {
	total := int64(math.MaxInt64)
	size := total/2 + 1
	start := size
	end := start + chunkLength(start, total, size) - 1
	if end != total-1 {
		t.Fatalf("range overflow: %d", end)
	}
}
func TestMergedArtifactPublication(t *testing.T) {
	for _, kind := range []string{"complete", "short"} {
		t.Run(kind, func(t *testing.T) {
			content := []byte("0123456789abcdefghij")
			server := mockServer(content, true)
			defer server.Close()
			cfg := DefaultConfig()
			cfg.OutputDir = t.TempDir()
			cfg.ResumeStateDir = t.TempDir()
			cfg.Network.PrivateOrigins = []string{server.URL}
			cfg.ChunkSize = 10
			d := NewDownloader(cfg)
			la := &labor{url: server.URL, filename: "out"}
			dir := d.segDir(la)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			for i := int64(0); i < 20; i += 10 {
				data := content[i : i+10]
				if kind == "short" && i == 10 {
					data = data[:2]
				}
				if err := os.WriteFile(chunkTempPath(dir, i), data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.saveResumeState(la, &ResumeState{URL: server.URL, TotalSize: 20, ChunkSize: 10, Completed: []int64{0, 10}}); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(cfg.OutputDir, "out")
			old := []byte("previous successful artifact")
			if err := os.WriteFile(target, old, 0644); err != nil {
				t.Fatal(err)
			}
			res := d.Download(context.Background(), server.URL, "", "out")
			want := content
			if kind == "short" {
				want = old
				if res.Error == nil {
					t.Fatal("short segment succeeded")
				}
			} else if res.Error != nil {
				t.Fatal(res.Error)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != string(want) {
				t.Fatalf("output %q: %v", got, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(cfg.OutputDir, ".papa-merge-*.tmp"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("merge temp leak: %v %v", leftovers, err)
			}
		})
	}
}

func TestMergedArtifactPreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not represented on Windows")
	}
	content := []byte("0123456789abcdefghij")
	server := mockServer(content, true)
	defer server.Close()
	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.Network.PrivateOrigins = []string{server.URL}
	cfg.ChunkSize = 10
	target := filepath.Join(cfg.OutputDir, "out")
	if err := os.WriteFile(target, []byte("private previous artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	result := NewDownloader(cfg).Download(context.Background(), server.URL, "", "out")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("replacement changed private output permissions: got %04o want 0600", got)
	}
}

func TestMergedPublicationFailureCanResume(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	server := mockServer(content, true)
	defer server.Close()
	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.Network.PrivateOrigins = []string{server.URL}
	cfg.ChunkSize = 10
	cfg.SaveBatchSize = 1
	d := NewDownloader(cfg)
	target := filepath.Join(cfg.OutputDir, "out")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "old")
	if err := os.WriteFile(marker, []byte("previous artifact"), 0644); err != nil {
		t.Fatal(err)
	}
	result := d.Download(context.Background(), server.URL, "", "out")
	if result.Error == nil {
		t.Fatal("publication over a nonempty directory succeeded")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "previous artifact" {
		t.Fatalf("previous artifact lost: %q %v", got, err)
	}
	la := &labor{url: server.URL, filename: "out"}
	state, err := d.loadResumeState(la)
	if err != nil || state == nil || len(state.Completed) != 2 {
		t.Fatalf("publication failure lost resume state: %+v %v", state, err)
	}
	for _, start := range []int64{0, 10} {
		if _, err := os.Stat(chunkTempPath(d.segDir(la), start)); err != nil {
			t.Fatal(err)
		}
	}
	if leftovers, err := filepath.Glob(filepath.Join(cfg.OutputDir, ".papa-merge-*.tmp")); err != nil || len(leftovers) != 0 {
		t.Fatalf("merge temp leak: %v %v", leftovers, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	result = d.Download(context.Background(), server.URL, "", "out")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(content) {
		t.Fatalf("resumed artifact: %q %v", got, err)
	}
}

func TestChunkCancellationDrainsProgressCallback(t *testing.T) {
	server := mockServer([]byte("0123456789abcdefghij"), true)
	defer server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.Network.PrivateOrigins = []string{server.URL}
	cfg.ChunkSize, cfg.MaxConcurrent = 10, 1
	callbacks := 0
	cfg.OnProgress = func(_, _ int64) { callbacks++; close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *DownloadResult, 1)
	go func() { done <- NewDownloader(cfg).Download(ctx, server.URL, "", "out") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("download returned before callback drained")
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case result := <-done:
		if !errors.Is(result.Error, context.Canceled) || callbacks != 1 {
			t.Fatalf("error=%v callbacks=%d", result.Error, callbacks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not drain")
	}
}
