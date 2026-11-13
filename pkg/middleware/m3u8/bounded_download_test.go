package m3u8

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxyOverlayTimeoutIsolation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SegmentTimeout = 7 * time.Second
	d := NewDownloader(cfg)
	base := d.clientFor(&DownloadOptions{Proxy: "http://127.0.0.1:9"})
	timeout := 1
	derived := d.With(Overlay{SegmentTimeout: &timeout}).clientFor(&DownloadOptions{Proxy: "http://127.0.0.1:9"})
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

func TestSegmentSchedulingBounded(t *testing.T) {
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&playlist, "#EXTINF:1,\n%d.ts\n", i)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list.m3u8" {
			fmt.Fprint(w, playlist.String())
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
	cfg.MaxConcurrent = 2
	cfg.MaxRetries = 0
	cfg.AutoMerge = false
	d := NewDownloader(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseline := runtime.NumGoroutine()
	done := make(chan *DownloadResult, 1)
	go func() { done <- d.Download(ctx, server.URL+"/list.m3u8", "", "out") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no segment request")
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
		t.Fatalf("unbounded goroutine growth: %d for concurrency 2", growth)
	}
}

func TestSegmentCancellationDrainsProgressCallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n0.ts\n#EXTINF:1,\n1.ts\n#EXT-X-ENDLIST\n")
			return
		}
		fmt.Fprint(w, "segment")
	}))
	defer server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.Network.PrivateOrigins = []string{server.URL}
	cfg.MaxConcurrent, cfg.AutoMerge = 1, false
	callbacks := 0
	cfg.OnProgress = func(_, _, _ int, _, _ int64) { callbacks++; close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *DownloadResult, 1)
	go func() { done <- NewDownloader(cfg).Download(ctx, server.URL+"/list.m3u8", "", "out") }()
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
