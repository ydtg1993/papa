package filedown

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestChunkBackoffExtremeInputs(t *testing.T) {
	for _, input := range [][2]int{{math.MaxInt, 1}, {1, math.MaxInt}, {64, 1}, {63, 1}} {
		seen := map[time.Duration]bool{}
		for range 64 {
			got := chunkBackoff(input[0], input[1])
			seen[got] = true
			if got < 5*time.Second || got > 10*time.Second {
				t.Fatalf("%v: %v", input, got)
			}
		}
		if len(seen) < 2 {
			t.Fatal("chunk policy did not jitter")
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

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryBody struct {
	io.Reader
	closed *int
}

func (b *retryBody) Close() error { (*b.closed)++; return nil }

func TestChunkSaturatedBackoffCancelsBeforeNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, closed := 0, 0
		cfg := DefaultConfig()
		cfg.RetryInterval = math.MaxInt
		d := NewDownloader(cfg)
		// No network goroutine: Wait can prove that only retry timing is pending.
		d.client = &http.Client{Transport: retryRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 500, Header: make(http.Header), Body: &retryBody{Reader: strings.NewReader(""), closed: &closed}, Request: r}, nil
		})}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var runErr error
		dest := filepath.Join(t.TempDir(), "chunk.part")
		go func() {
			defer close(done)
			runErr = d.downloadChunkToFile(ctx, "https://fixture.invalid/file", 0, 9, dest, &requestConfig{})
		}()
		t.Cleanup(func() {
			cancel()
			synctest.Wait()
			select {
			case <-done:
			default:
				time.Sleep(11 * time.Second)
				synctest.Wait()
			}
		})
		synctest.Wait()
		if calls != 1 || closed != 1 {
			t.Fatalf("request not completed: calls=%d closed=%d", calls, closed)
		}
		select {
		case <-done:
			t.Fatalf("not waiting: %v", runErr)
		default:
		}
		before := time.Now()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("backoff ignored cancellation")
		}
		if !errors.Is(runErr, context.Canceled) || !time.Now().Equal(before) || calls != 1 {
			t.Fatalf("cancel err=%v elapsed=%v calls=%d", runErr, time.Since(before), calls)
		}
		for _, path := range []string{dest, dest + ".tmp"} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("left %s: %v", path, err)
			}
		}
	})
}

func TestChunkRetryTimingUsesPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.MaxRetries = 2
		cfg.RetryInterval = 1
		d := NewDownloader(cfg)
		var at []time.Time
		d.client = &http.Client{Transport: retryRoundTripper(func(r *http.Request) (*http.Response, error) {
			at = append(at, time.Now())
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}
		start := time.Now()
		err := d.downloadChunkToFile(context.Background(), "https://fixture.invalid/file", 0, 9, filepath.Join(t.TempDir(), "chunk"), &requestConfig{})
		if err == nil || len(at) != 3 {
			t.Fatalf("err=%v calls=%d", err, len(at))
		}
		if !at[0].Equal(start) || !time.Now().Equal(at[2]) {
			t.Fatal("wait before first or after final attempt")
		}
		for i := 1; i < 3; i++ {
			bound := time.Duration(i) * 2 * time.Second
			gap := at[i].Sub(at[i-1])
			if gap < bound/2 || gap > bound {
				t.Fatalf("retry %d gap=%v", i, gap)
			}
		}
	})
}
