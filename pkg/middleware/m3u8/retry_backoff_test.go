package m3u8

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
	"github.com/ydtg1993/papa/v4/pkg/middleware"
)

func TestM3U8RetryBackoffPolicies(t *testing.T) {
	for _, tc := range []struct {
		attempt, base    int
		request, segment time.Duration
	}{
		{1, 2, 2 * time.Second, 2 * time.Second}, {2, 3, 6 * time.Second, 4 * time.Second},
		{1, 0, 2 * time.Second, 2 * time.Second}, {1, -1, 2 * time.Second, 2 * time.Second},
		{1, math.MaxInt, 10 * time.Second, 2 * time.Second}, {math.MaxInt, 1, 10 * time.Second, 10 * time.Second},
	} {
		for range 64 {
			if got := requestBackoff(tc.attempt, tc.base); got < tc.request/2 || got > tc.request {
				t.Fatalf("request %+v: %v", tc, got)
			}
			if got := segmentBackoff(tc.attempt); got < tc.segment/2 || got > tc.segment {
				t.Fatalf("segment %+v: %v", tc, got)
			}
		}
	}
}

func TestJitteredSegmentRetryCountsAndCleanup(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "encrypted"}[encrypted], func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer srv.Close()
			cfg := DefaultConfig()
			cfg.Network = egress.Config{PrivateOrigins: []string{srv.URL}}
			cfg.MaxRetries, cfg.RetryInterval = 1, 1
			d := NewDownloader(cfg)
			d.budget = middleware.NewDownloadBudget(3*d.limits.MediaBytes, 1<<30)
			var key *KeyInfo
			want := int32(2)
			if encrypted {
				key = &KeyInfo{}
				want = 4
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dest := filepath.Join(t.TempDir(), "segment.ts")
			err := d.downloadSegmentToFile(ctx, &SegmentInfo{URL: srv.URL}, key, nil, dest, nil)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected exhausted retries: %v", err)
			}
			if calls.Load() != want {
				t.Fatalf("requests=%d want=%d", calls.Load(), want)
			}
			for _, path := range []string{dest, dest + ".tmp"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("left %s: %v", path, err)
				}
			}
			release, err := d.budget.AcquireMemory(3 * d.limits.MediaBytes)
			if err != nil {
				t.Fatalf("retry leaked memory reservation: %v", err)
			}
			release()
		})
	}
}

func TestM3U8SaturatedRequestBackoffCancels(t *testing.T) {
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
	d.budget = middleware.NewDownloadBudget(3*d.limits.MediaBytes, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := d.doRequest(ctx, srv.URL, "", nil); done <- err }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("request leaked")
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
		t.Fatal("backoff ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("cancel retried: %d", calls.Load())
	}
	release, err := d.budget.AcquireMemory(3 * d.limits.MediaBytes)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestEncryptedOuterBackoffCancellationReleasesBudget(t *testing.T) {
	var calls atomic.Int32
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte("fixture"))
		select {
		case first <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.Network = egress.Config{PrivateOrigins: []string{srv.URL}}
	d := NewDownloader(cfg)
	d.budget = middleware.NewDownloadBudget(3*d.limits.MediaBytes, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	dest := filepath.Join(t.TempDir(), "segment.ts")
	// The successful HTTP response followed by invalid IV reaches the outer
	// segment retry, without spending time inside request-level retries.
	go func() {
		done <- d.downloadSegmentToFile(ctx, &SegmentInfo{URL: srv.URL}, &KeyInfo{IV: "invalid"}, nil, dest, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("segment leaked")
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
		t.Fatalf("outer retry exited early: %v", err)
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
		t.Fatal("outer backoff ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("outer cancellation retried: %d", calls.Load())
	}
	for _, path := range []string{dest, dest + ".tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("left %s: %v", path, err)
		}
	}
	release, err := d.budget.AcquireMemory(3 * d.limits.MediaBytes)
	if err != nil {
		t.Fatalf("cancel leaked reservation: %v", err)
	}
	release()
}
