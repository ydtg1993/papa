package filedown

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func receiveDownload(t *testing.T, result <-chan *DownloadResult) *DownloadResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("download did not finish")
		return nil
	}
}

func TestDifferentSourcesSameDestination(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "direct", false: "chunks"}[direct], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := "AAAA"
				if r.URL.Path == "/b" {
					body = "BBBB"
				}
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Method == http.MethodHead {
					return
				}
				if r.Header.Get("Range") != "" {
					w.Header().Set("Content-Range", "bytes 0-3/4")
					w.WriteHeader(http.StatusPartialContent)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
			cfg.ChunkSize = 4
			heldA, heldB := make(chan struct{}), make(chan struct{})
			releaseA, releaseB := make(chan struct{}), make(chan struct{})
			var onceA, onceB sync.Once
			defer onceA.Do(func() { close(releaseA) })
			defer onceB.Do(func() { close(releaseB) })
			var callbacks atomic.Int32
			cfg.OnProgress = func(_, _ int64) {
				switch callbacks.Add(1) {
				case 1:
					close(heldA)
					<-releaseA
				case 2:
					close(heldB)
					<-releaseB
				}
			}
			d := NewDownloader(cfg)
			// The roots differ textually, but both calls write nested/same.bin.
			alias := filepath.Join(cfg.OutputDir, "nested")
			fork := d.With(Overlay{OutputDir: &alias})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a, b := make(chan *DownloadResult, 1), make(chan *DownloadResult, 1)
			go func() { a <- d.Download(ctx, server.URL+"/a", "nested", "same.bin", &DownloadOptions{Direct: direct}) }()
			select {
			case <-heldA:
			case <-ctx.Done():
				t.Fatal("first download did not reach progress callback")
			}
			go func() { b <- fork.Download(ctx, server.URL+"/b", "", "same.bin", &DownloadOptions{Direct: direct}) }()
			select {
			case <-heldB:
				t.Error("different sources wrote the same destination concurrently")
			case <-time.After(150 * time.Millisecond):
			}
			onceA.Do(func() { close(releaseA) })
			if got := receiveDownload(t, a); got.Error != nil {
				t.Errorf("source A: %v", got.Error)
			}
			if data, err := os.ReadFile(filepath.Join(alias, "same.bin")); err != nil || string(data) != "AAAA" {
				t.Errorf("source A reported success with content %q, read error %v", data, err)
			}
			onceB.Do(func() { close(releaseB) })
			if got := receiveDownload(t, b); got.Error != nil {
				t.Fatalf("source B: %v", got.Error)
			}
			if data, err := os.ReadFile(filepath.Join(alias, "same.bin")); err != nil || string(data) != "BBBB" {
				t.Fatalf("source B used the wrong source: %q, %v", data, err)
			}
		})
	}
}

func TestDestinationWaitCanBeCanceled(t *testing.T) {
	for _, source := range []string{"/a", "/b"} {
		t.Run(source, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte("data"))
			}))
			defer server.Close()
			cfg := DefaultConfig()
			cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
			held, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var callbacks atomic.Int32
			cfg.OnProgress = func(_, _ int64) {
				if callbacks.Add(1) == 1 {
					close(held)
					<-release
				}
			}
			d := NewDownloader(cfg)
			a, b := make(chan *DownloadResult, 1), make(chan *DownloadResult, 1)
			go func() {
				a <- d.Download(context.Background(), server.URL+"/a", "", "same.bin", &DownloadOptions{Direct: true})
			}()
			<-held
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			go func() {
				b <- d.With(Overlay{}).Download(ctx, server.URL+source, "", "same.bin", &DownloadOptions{Direct: true})
			}()
			select {
			case got := <-b:
				if !errors.Is(got.Error, context.DeadlineExceeded) {
					t.Errorf("wait returned %v, want context.DeadlineExceeded", got.Error)
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("canceled waiter remained blocked by active download")
			}
			once.Do(func() { close(release) })
			if got := receiveDownload(t, a); got.Error != nil {
				t.Fatal(got.Error)
			}
			if requests.Load() != 1 {
				t.Fatalf("canceled download sent a request: count=%d", requests.Load())
			}
		})
	}
}

func TestWithIndependentOutputAndResumeRootsRunConcurrently(t *testing.T) {
	held := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held <- struct{}{}
		<-release
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()
	var once sync.Once
	defer once.Do(func() { close(release) })
	cfg := DefaultConfig()
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	d := NewDownloader(cfg)
	otherRoot, otherResume := t.TempDir(), t.TempDir()
	fork := d.With(Overlay{OutputDir: &otherRoot, ResumeStateDir: &otherResume})
	results := make(chan *DownloadResult, 2)
	for _, dl := range []*Downloader{d, fork} {
		go func() {
			results <- dl.Download(context.Background(), server.URL, "", "file.bin", &DownloadOptions{Direct: true})
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-held:
		case <-time.After(300 * time.Millisecond):
			t.Error("independent output and resume roots were serialized")
		}
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if got := receiveDownload(t, results); got.Error != nil {
			t.Fatal(got.Error)
		}
	}
}

func TestTemporaryFileDoesNotOverwriteAnotherDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var callbacks atomic.Int32
	cfg.OnProgress = func(_, _ int64) {
		if callbacks.Add(1) == 1 {
			close(held)
			<-release
		}
	}
	d := NewDownloader(cfg)
	resultA := make(chan *DownloadResult, 1)
	go func() {
		resultA <- d.Download(context.Background(), server.URL+"/a", "", "same.bin", &DownloadOptions{Direct: true})
	}()
	<-held
	if got := d.Download(context.Background(), server.URL+"/b", "", "same.bin.tmp", &DownloadOptions{Direct: true}); got.Error != nil {
		t.Errorf("second destination: %v", got.Error)
	}
	once.Do(func() { close(release) })
	if got := receiveDownload(t, resultA); got.Error != nil {
		t.Fatal(got.Error)
	}
	for name, want := range map[string]string{"same.bin": "/a", "same.bin.tmp": "/b"} {
		if data, err := os.ReadFile(filepath.Join(cfg.OutputDir, name)); err != nil || string(data) != want {
			t.Errorf("destination %s: got %q, want %q, error=%v", name, data, want, err)
		}
	}
}
