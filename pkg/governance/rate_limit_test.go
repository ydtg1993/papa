package governance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestoreRetryAfterExtendsDeadlineWithoutEnablingLocalPolicy(t *testing.T) {
	clock := newClock()
	c := controller(t, DefaultConfig(), clock)
	deadline := clock.Now().Add(10 * time.Second)
	c.RestoreRetryAfter("fixture", deadline)
	c.RestoreRetryAfter("fixture", clock.Now().Add(5*time.Second))
	c.RestoreRetryAfter("fixture", clock.Now().Add(-time.Second))
	if got := c.RetryAfterUntil("fixture"); !got.Equal(deadline) {
		t.Fatalf("restore shortened deadline: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := asyncRequest(c, ctx, "fixture")
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	clock.advance(9 * time.Second)
	c.Observe("fixture", Feedback{})
	assertRequestBlocked(t, ch)
	clock.advance(time.Second)
	c.Observe("fixture", Feedback{})
	takeRequest(t, ch)(Feedback{})
	if p := c.Effective("fixture").Request; p.Enabled || p.AutoThrottle.Enabled {
		t.Fatal("restoring remote protection enabled local policy")
	}
	if snap := c.Snapshot().Sites[0]; snap.Errors != 0 || snap.RateLimited != 0 {
		t.Fatalf("restoring deadline fabricated response observations: %+v", snap)
	}
}

func TestRestoreRetryAfterPreservesRemoteWaitBudget(t *testing.T) {
	clock := newClock()
	cfg := DefaultConfig()
	cfg.Defaults.Request.RetryAfterMax = time.Minute
	c := controller(t, cfg, clock)
	c.RestoreRetryAfter("fixture", clock.Now().Add(2*time.Minute))
	if _, err := c.AcquireRequest(context.Background(), "fixture"); !errors.Is(err, ErrRemoteWaitExceeded) {
		t.Fatalf("restored excessive wait: %v", err)
	}
	c.AcknowledgeRemote("fixture")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := asyncRequest(c, ctx, "fixture")
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	clock.advance(time.Minute)
	c.Observe("fixture", Feedback{})
	assertRequestBlocked(t, ch)
	clock.advance(time.Minute)
	c.Observe("fixture", Feedback{})
	takeRequest(t, ch)(Feedback{})
}

func TestRetryAfterHonoredWithLocalControlsDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, status := range []int{429, 503} {
			for _, date := range []bool{false, true} {
				t.Run(fmt.Sprintf("request=%v/status=%d/date=%v", enabled, status, date), func(t *testing.T) {
					clock := newClock()
					cfg := DefaultConfig()
					cfg.Defaults.Request.Enabled = enabled
					c := controller(t, cfg, clock)
					header := "10"
					deadline := clock.Now().Add(10 * time.Second)
					if date {
						header = deadline.Format(http.TimeFormat)
					}
					c.Observe("a", Feedback{StatusCode: status, RetryAfter: header})
					if got := c.RetryAfterUntil("a"); !got.Equal(deadline) {
						t.Fatalf("deadline %v, want %v", got, deadline)
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					ch := asyncRequest(c, ctx, "a")
					eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
					clock.advance(9 * time.Second)
					c.Observe("a", Feedback{})
					assertRequestBlocked(t, ch)
					clock.advance(time.Second)
					c.Observe("a", Feedback{})
					takeRequest(t, ch)(Feedback{})
					if c.Effective("a").Request.AutoThrottle.Enabled {
						t.Fatal("AutoThrottle enabled implicitly")
					}
				})
			}
		}
	}
}

func TestInvalidRetryAfterDoesNotEnableAutomaticCooldown(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, header := range []string{"", "nonsense", "-1"} {
			cfg := DefaultConfig()
			cfg.Defaults.Request.Enabled = enabled
			c := controller(t, cfg, newClock())
			c.Observe("a", Feedback{StatusCode: 429, RetryAfter: header})
			if !c.RetryAfterUntil("a").IsZero() {
				t.Fatalf("enabled=%v header=%q invented cooldown", enabled, header)
			}
			if c.Snapshot().Sites[0].RateLimited != 1 {
				t.Fatal("missing rate-limit observation")
			}
			takeRequest(t, asyncRequest(c, context.Background(), "a"))(Feedback{})
		}
	}
}

func TestAutoThrottleBacksOffFastRejectionsWithinBounds(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	cfg.Defaults.Request.AutoThrottle.Enabled = true
	cfg.Defaults.Request.AutoThrottle.MaxDelay = 500 * time.Millisecond
	c := controller(t, cfg, clock)
	for i, want := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond} {
		c.Observe("a", Feedback{StatusCode: 429, Latency: time.Duration(i)})
		if got := c.Snapshot().Sites[0].EffectiveDelay; got != want {
			t.Fatalf("rejection %d delay %v want %v", i, got, want)
		}
	}
	if got := c.RetryAfterUntil("a"); !got.Equal(clock.Now().Add(cfg.Defaults.Request.RetryAfterDefault)) {
		t.Fatalf("missing enabled fallback: %v", got)
	}
}

func TestTransportHonorsRetryAfterWhenRequestPolicyDisabled(t *testing.T) {
	clock := newClock()
	c := controller(t, DefaultConfig(), clock)
	var calls atomic.Int32
	tr := WrapTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		h := make(http.Header)
		if calls.Add(1) == 1 {
			status = 429
			h.Set("Retry-After", clock.Now().Add(10*time.Second).Format(http.TimeFormat))
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("fixture")), Request: r}, nil
	}))
	ctx, cancel := context.WithCancel(requestContext(c))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	done := make(chan error, 1)
	go func() {
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	clock.advance(9 * time.Second)
	c.Observe("fixture", Feedback{})
	if calls.Load() != 1 {
		t.Fatal("HTTP request sent before Retry-After deadline")
	}
	clock.advance(time.Second)
	c.Observe("fixture", Feedback{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not resume")
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected request replay")
	}
}
