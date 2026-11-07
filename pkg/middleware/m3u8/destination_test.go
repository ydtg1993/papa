package m3u8

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path.Clean(r.URL.Path) {
		case "/a.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n"))
		case "/b.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\nb.ts\n#EXT-X-ENDLIST\n"))
		case "/a.ts":
			_, _ = w.Write([]byte("AAAA"))
		case "/b.ts":
			_, _ = w.Write([]byte("BBBB"))
		}
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{server.URL}}
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	cfg.AutoMerge = false
	heldA, heldB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	var onceA, onceB sync.Once
	defer onceA.Do(func() { close(releaseA) })
	defer onceB.Do(func() { close(releaseB) })
	var callbacks atomic.Int32
	cfg.OnProgress = func(_, _, _ int, _, _ int64) {
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
	alias := filepath.Join(cfg.OutputDir, "nested")
	fork := d.With(Overlay{OutputDir: &alias})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := make(chan *DownloadResult, 1), make(chan *DownloadResult, 1)
	go func() { a <- d.Download(ctx, server.URL+"/a.m3u8", "nested", "same.ts") }()
	select {
	case <-heldA:
	case <-ctx.Done():
		t.Fatal("first download did not reach progress callback")
	}
	go func() { b <- fork.Download(ctx, server.URL+"/b.m3u8", "", "same.ts") }()
	select {
	case <-heldB:
		t.Error("different sources wrote the same destination concurrently")
	case <-time.After(150 * time.Millisecond):
	}
	onceA.Do(func() { close(releaseA) })
	if got := receiveDownload(t, a); got.Error != nil {
		t.Errorf("source A: %v", got.Error)
	}
	if data, err := os.ReadFile(filepath.Join(alias, "same.ts")); err != nil || string(data) != "AAAA" {
		t.Errorf("source A reported success with content %q, read error %v", data, err)
	}
	onceB.Do(func() { close(releaseB) })
	if got := receiveDownload(t, b); got.Error != nil {
		t.Fatalf("source B: %v", got.Error)
	}
	if data, err := os.ReadFile(filepath.Join(alias, "same.ts")); err != nil || string(data) != "BBBB" {
		t.Fatalf("source B used the wrong source: %q, %v", data, err)
	}
}

func TestDestinationWaitCanBeCanceled(t *testing.T) {
	for _, source := range []string{"/a", "/b"} {
		t.Run(source, func(t *testing.T) {
			var requests atomic.Int32
			held, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(held)
					<-release
				}
				_, _ = w.Write([]byte("#EXTM3U\n"))
			}))
			defer server.Close()
			defer once.Do(func() { close(release) })
			cfg := DefaultConfig()
			cfg.Network = egress.Config{PrivateOrigins: []string{server.URL}}
			cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
			cfg.OutputMode = OutputPlaylist
			d := NewDownloader(cfg)
			a, b := make(chan *DownloadResult, 1), make(chan *DownloadResult, 1)
			go func() { a <- d.Download(context.Background(), server.URL+"/a", "", "same.m3u8") }()
			<-held
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			go func() { b <- d.With(Overlay{}).Download(ctx, server.URL+source, "", "same.m3u8") }()
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

func TestDestinationLockUsesMergeExtension(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/a.m3u8" {
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n"))
		} else {
			_, _ = w.Write([]byte("AAAA"))
		}
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{server.URL}}
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	cfg.OnProgress = func(_, _, _ int, _, _ int64) { close(held); <-release }
	d := NewDownloader(cfg)
	a, b := make(chan *DownloadResult, 1), make(chan *DownloadResult, 1)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ext := ".mkv"
	go func() { a <- d.Download(ctxA, server.URL+"/a.m3u8", "", "movie.ts", &DownloadOptions{MergeExt: &ext}) }()
	<-held
	ctxB, cancelB := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelB()
	playlist := OutputPlaylist
	go func() {
		b <- d.Download(ctxB, server.URL+"/b.m3u8", "", "movie.mkv", &DownloadOptions{Output: &playlist})
	}()
	if got := receiveDownload(t, b); !errors.Is(got.Error, context.DeadlineExceeded) {
		t.Errorf("actual merge destination was not locked: result=%+v", got)
	}
	if requests.Load() != 2 {
		t.Errorf("waiting merge destination sent a request: count=%d", requests.Load())
	}
	cancelA()
	once.Do(func() { close(release) })
	if got := receiveDownload(t, a); got.Error == nil {
		t.Fatal("canceled merge unexpectedly succeeded")
	}
}

func TestSegmentsDirectoryBlocksChildDestination(t *testing.T) {
	var childRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path.Clean(r.URL.Path) {
		case "/a.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST\n"))
		case "/b.m3u8":
			childRequests.Add(1)
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\nb.ts\n#EXT-X-ENDLIST\n"))
		case "/a.ts":
			_, _ = w.Write([]byte("AAAA"))
		case "/b.ts":
			_, _ = w.Write([]byte("BBBB"))
		case "/independent.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n"))
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var callbacks atomic.Int32
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{server.URL}}
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	cfg.AutoMerge = false
	cfg.OnProgress = func(_, _, _ int, _, _ int64) {
		if callbacks.Add(1) == 1 {
			close(held)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	d := NewDownloader(cfg)
	segments := OutputSegments
	a := make(chan *DownloadResult, 1)
	go func() {
		a <- d.Download(ctx, server.URL+"/a.m3u8", "", "movie", &DownloadOptions{Output: &segments})
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("first download did not reach progress callback")
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 100*time.Millisecond)
	b := d.With(Overlay{}).Download(waitCtx, server.URL+"/b.m3u8", "movie", "segment_00000.ts")
	cancelWait()
	if !errors.Is(b.Error, context.DeadlineExceeded) || childRequests.Load() != 0 {
		t.Errorf("child destination bypassed directory lock: error=%v requests=%d", b.Error, childRequests.Load())
	}
	// A shared parent alone must not serialize independent output resources.
	playlist := OutputPlaylist
	if result := d.Download(ctx, server.URL+"/independent.m3u8", "", "movie-other.m3u8", &DownloadOptions{Output: &playlist}); result.Error != nil {
		t.Error("independent destination was blocked:", result.Error)
	}
	once.Do(func() { close(release) })
	if result := receiveDownload(t, a); result.Error != nil {
		t.Fatal(result.Error)
	}
	output := filepath.Join(cfg.OutputDir, "movie", "segment_00000.ts")
	if data, err := os.ReadFile(output); err != nil || string(data) != "AAAA" {
		t.Fatalf("segment output=%q err=%v", data, err)
	}
	// After A finishes, a fresh child download succeeds; the canceled waiter
	// must not retain either the output path or its resume resource locks.
	if result := d.With(Overlay{}).Download(ctx, server.URL+"/b.m3u8", "movie", "segment_00000.ts"); result.Error != nil {
		t.Fatal(result.Error)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "BBBB" {
		t.Fatalf("subsequent child output=%q err=%v", data, err)
	}
}
