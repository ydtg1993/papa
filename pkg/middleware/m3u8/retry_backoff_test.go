package m3u8

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
		requests, segments := map[time.Duration]bool{}, map[time.Duration]bool{}
		for range 64 {
			got := requestBackoff(tc.attempt, tc.base)
			requests[got] = true
			if got < tc.request/2 || got > tc.request {
				t.Fatalf("request %+v: %v", tc, got)
			}
			got = segmentBackoff(tc.attempt)
			segments[got] = true
			if got < tc.segment/2 || got > tc.segment {
				t.Fatalf("segment %+v: %v", tc, got)
			}
		}
		if len(requests) < 2 || len(segments) < 2 {
			t.Fatal("request/segment policy did not jitter")
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

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryBody struct {
	io.Reader
	closed *int
}

func (b *retryBody) Close() error { (*b.closed)++; return nil }

func TestM3U8SaturatedRequestBackoffCancels(t *testing.T) { testRetryCancellation(t, false, false) }
func TestEncryptedOuterBackoffCancellationReleasesBudget(t *testing.T) {
	testRetryCancellation(t, true, false)
}
func TestM3U8RetryBackoffDeadline(t *testing.T) {
	t.Run("request", func(t *testing.T) { testRetryCancellation(t, false, true) })
	t.Run("outer", func(t *testing.T) { testRetryCancellation(t, true, true) })
}

func testRetryCancellation(t *testing.T, outer, deadline bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		calls, closed := 0, 0
		cfg := DefaultConfig()
		cfg.RetryInterval = math.MaxInt
		d := NewDownloader(cfg)
		d.budget = middleware.NewDownloadBudget(3*d.limits.MediaBytes, 1<<30)
		d.client = &http.Client{Transport: retryRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			status := 500
			if outer {
				status = 200
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: &retryBody{Reader: strings.NewReader("fixture"), closed: &closed}, ContentLength: 7, Request: r}, nil
		})}
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		done := make(chan struct{})
		var runErr error
		dest := filepath.Join(t.TempDir(), "segment.ts")
		go func() {
			defer close(done)
			if outer {
				runErr = d.downloadSegmentToFile(ctx, &SegmentInfo{URL: "https://fixture.invalid/segment"}, &KeyInfo{IV: "invalid"}, nil, dest, nil)
			} else {
				_, runErr = d.doRequest(ctx, "https://fixture.invalid/segment", "", nil)
			}
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
		// With a successful closed response and invalid IV, only the encrypted
		// outer loop can be waiting. Otherwise the 500 enters request-level wait.
		before := time.Now()
		wantErr := error(context.Canceled)
		wantElapsed := time.Duration(0)
		if deadline {
			wantErr = context.DeadlineExceeded
			wantElapsed = 500 * time.Millisecond
			time.Sleep(wantElapsed)
		} else {
			cancel()
		}
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("backoff ignored cancellation/deadline")
		}
		if !errors.Is(runErr, wantErr) || time.Since(before) != wantElapsed || calls != 1 {
			t.Fatalf("err=%v elapsed=%v calls=%d", runErr, time.Since(before), calls)
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
	})
}

func TestM3U8RetryTimingUsesPolicy(t *testing.T) {
	for _, outer := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "outer"}[outer], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.MaxRetries = 2
				cfg.RetryInterval = 3
				d := NewDownloader(cfg)
				var at []time.Time
				d.client = &http.Client{Transport: retryRoundTripper(func(r *http.Request) (*http.Response, error) {
					at = append(at, time.Now())
					status := 500
					if outer {
						status = 200
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("fixture")), ContentLength: 7, Request: r}, nil
				})}
				start := time.Now()
				var err error
				if outer {
					err = d.downloadSegmentToFile(context.Background(), &SegmentInfo{URL: "https://fixture.invalid/segment"}, &KeyInfo{IV: "invalid"}, nil, filepath.Join(t.TempDir(), "segment.ts"), nil)
				} else {
					_, err = d.doRequest(context.Background(), "https://fixture.invalid/segment", "", nil)
				}
				if err == nil || len(at) != 3 {
					t.Fatalf("err=%v calls=%d", err, len(at))
				}
				if !at[0].Equal(start) || !time.Now().Equal(at[2]) {
					t.Fatal("wait before first or after final attempt")
				}
				for i := 1; i < 3; i++ {
					bound := time.Duration(i) * 3 * time.Second
					if outer {
						bound = time.Duration(i) * 2 * time.Second
					}
					gap := at[i].Sub(at[i-1])
					if gap < bound/2 || gap > bound {
						t.Fatalf("retry %d gap=%v", i, gap)
					}
				}
			})
		})
	}
}
