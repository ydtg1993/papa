package filedown

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestChunkBackoffExtremeInputs(t *testing.T) {
	for _, input := range [][2]int{{math.MaxInt, 1}, {1, math.MaxInt}, {64, 1}, {63, 1}} {
		for range 64 {
			if got := chunkBackoff(input[0], input[1]); got < 5*time.Second || got > 10*time.Second {
				t.Fatalf("%v: %v", input, got)
			}
		}
	}
}

func TestChunkJitterKeepsRetryCountAndNoPartialFile(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{srv.URL}}
	cfg.MaxRetries = 1
	d := NewDownloader(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "chunk.part")
	if err := d.downloadChunkToFile(ctx, srv.URL, 0, 9, dest, &requestConfig{}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected exhausted retries: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests=%d, want 2", calls.Load())
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed download published file: %v", err)
	}
}

func TestChunkSaturatedBackoffCancelsBeforeNextRequest(t *testing.T) {
	var calls atomic.Int32
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
		select {
		case first <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{srv.URL}}
	cfg.RetryInterval = math.MaxInt
	d := NewDownloader(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	dest := filepath.Join(t.TempDir(), "chunk.part")
	go func() { done <- d.downloadChunkToFile(ctx, srv.URL, 0, 9, dest, &requestConfig{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("download leaked")
		}
	})
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no first request")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("saturated retry exited early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		done <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not interrupt backoff")
	}
	if calls.Load() != 1 {
		t.Fatalf("cancel retried: %d", calls.Load())
	}
}
