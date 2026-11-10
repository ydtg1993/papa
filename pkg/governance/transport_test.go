package governance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type transparentTransport struct{ base http.RoundTripper }

func (t *transparentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r)
}
func (t *transparentTransport) BaseTransport() http.RoundTripper { return t.base }
func requestContext(c *Controller) context.Context {
	return WithSite(WithController(context.Background(), c), "fixture")
}

func TestTransportRedirectBodyLifecycleAndNoPostReplay(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/ok", 302)
		case "/limited":
			posts.Add(1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer server.Close()
	c := controller(t, requestConfig(), newClock())
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	wrapped := WrapTransport(base)
	if WrapTransport(wrapped) != wrapped {
		t.Fatal("wrapper was nested")
	}
	if wrapped.(interface{ BaseTransport() http.RoundTripper }).BaseTransport() != base {
		t.Fatal("cannot unwrap transport")
	}
	client := &http.Client{Transport: wrapped}
	req, _ := http.NewRequestWithContext(requestContext(c), "GET", server.URL+"/redirect", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot().Sites[0]; s.Requests != 2 || s.Inflight != 1 {
		t.Fatalf("redirect did not reacquire or response prematurely released: %+v", s)
	}
	io.Copy(io.Discard, resp.Body)
	if c.Snapshot().Sites[0].Inflight != 0 {
		t.Fatal("EOF did not release")
	}
	resp.Body.Close()
	req, _ = http.NewRequestWithContext(requestContext(c), "POST", server.URL+"/limited", strings.NewReader("payload"))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if posts.Load() != 1 || c.Snapshot().Sites[0].RateLimited != 1 {
		t.Fatal("rate limit replayed POST or omitted observation")
	}
}

func TestTransportErrorsPanicCancellationAndExcludedPermitWait(t *testing.T) {
	clock := newClock()
	cfg := requestConfig()
	cfg.Defaults.Request.ManualDelay = time.Minute
	c := controller(t, cfg, clock)
	tr := WrapTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("body"))}, nil
	}))
	req, _ := http.NewRequestWithContext(requestContext(c), "GET", "http://fixture.invalid", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithCancel(requestContext(c))
	req, _ = http.NewRequestWithContext(ctx, "GET", "http://fixture.invalid", nil)
	done := make(chan *http.Response, 1)
	go func() {
		r, e := tr.RoundTrip(req)
		if e != nil {
			t.Error(e)
		}
		done <- r
	}()
	eventually(t, func() bool { return c.Snapshot().Sites[0].Waiting == 1 })
	clock.advance(time.Minute)
	c.Observe("fixture", Feedback{})
	select {
	case resp = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request not admitted")
	}
	if got := c.Snapshot().Sites[0].LastLatency; got >= time.Second {
		t.Fatalf("permission wait leaked into latency: %v", got)
	}
	cancel()
	eventually(t, func() bool { return c.Snapshot().Sites[0].Inflight == 0 })
	resp.Body.Close()
	cfg.Defaults.Request.ManualDelay = 0
	c.Update(cfg)
	fail := WrapTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fixture failure") }))
	req, _ = http.NewRequestWithContext(requestContext(c), "GET", "http://fixture.invalid", nil)
	if _, err := fail.RoundTrip(req); err == nil {
		t.Fatal("missing error")
	}
	panicTransport := WrapTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { panic("fixture") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		panicTransport.RoundTrip(req)
	}()
	if c.Snapshot().Sites[0].Inflight != 0 {
		t.Fatal("error/panic leaked request permits")
	}
}

func TestTransportWithoutExplicitSiteDoesNotCreatePolicyState(t *testing.T) {
	c := controller(t, requestConfig(), newClock())
	tr := c.WrapTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}))
	req, _ := http.NewRequest("GET", "http://fixture.invalid", nil)
	tr.RoundTrip(req)
	if len(c.Snapshot().Sites) != 0 {
		t.Fatal("unscoped request created an implicit site")
	}
}

func TestRequestRateUsesBoundedWindowAndReportsUnsampled(t *testing.T) {
	clock := newClock()
	c := controller(t, DefaultConfig(), clock)
	release, _ := c.AcquireRequest(context.Background(), "a")
	release(Feedback{})
	if c.Snapshot().Sites[0].RateSampled {
		t.Fatal("zero-duration window presented as measured rate")
	}
	clock.advance(2 * time.Second)
	s := c.Snapshot().Sites[0]
	if !s.RateSampled || s.RequestsPerSecond != .5 || s.RateWindow != 2*time.Second {
		t.Fatalf("window rate=%+v", s)
	}
	clock.advance(time.Minute)
	s = c.Snapshot().Sites[0]
	if s.RequestsPerSecond != 0 || s.RateWindow != time.Minute {
		t.Fatalf("expired request remained in window: %+v", s)
	}
}

func TestNestedTransportWrappingDoesNotReacquireOnePermit(t *testing.T) {
	cfg := requestConfig()
	cfg.Defaults.Request.MaxInflight = 1
	c := controller(t, cfg, newClock())
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	chain := &transparentTransport{base: WrapTransport(base)}
	if wrapped := WrapTransport(chain); wrapped != chain {
		t.Fatal("added duplicate governance beneath transparent wrapper")
	}
	ctx, cancel := context.WithTimeout(requestContext(c), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://fixture.invalid", nil)
	if _, err := WrapTransport(chain).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	// Opaque wrappers cannot be inspected; the per-exchange context marker still
	// prevents a duplicate admission deadlock at max_inflight=1.
	opaque := roundTripFunc(chain.RoundTrip)
	if _, err := WrapTransport(opaque).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot().Sites[0]; got.Requests != 2 || got.Inflight != 0 {
		t.Fatalf("double counted nested exchange: %+v", got)
	}
}
