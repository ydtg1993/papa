package proxy

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

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func testHealthClient(t *testing.T, m *Manager, origins ...string) *http.Client {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = RequestProxy
	c := egress.Client(&http.Client{Transport: tr, Timeout: time.Second}, egress.Config{PrivateOrigins: origins})
	c.Transport = WrapTransport(c.Transport, m)
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestTransportProxyAuthFailureCannotFallBackDirect(t *testing.T) {
	var targetHits, proxyHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer upstream.Close()
	m, _ := healthyManager(t, upstream.URL)
	c := testHealthClient(t, m, target.URL)
	if _, err := c.Get(target.URL); err == nil {
		t.Fatal("proxy auth rejection succeeded")
	}
	if s := m.Snapshot()[0]; s.State != StateCooling || s.LastResult != AuthenticationFailure || s.InFlight != 0 {
		t.Fatalf("actual CONNECT407 not reported: %+v", s)
	}
	if _, err := c.Get(target.URL); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("exhaustion did not fail closed: %v", err)
	}
	if targetHits.Load() != 0 || proxyHits.Load() != 1 {
		t.Fatalf("unexpected egress target=%d proxy=%d", targetHits.Load(), proxyHits.Load())
	}
	ctx := WithPool(context.Background(), false)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if targetHits.Load() != 1 {
		t.Fatal("explicit direct opt-out no longer works")
	}
	// A fixed proxy takes priority over an explicit pool opt-out and must fail
	// closed without borrowing another identity or becoming direct.
	req, _ = http.NewRequestWithContext(core.WithProxyURL(ctx, upstream.URL), http.MethodGet, target.URL, nil)
	if _, err := c.Do(req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fixed cooled proxy bypassed: %v", err)
	}
}

func TestTransportTargetFailuresStayNeutralAndIdentityDrainsWithBody(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, "target body")
	}))
	defer target.Close()
	upstream, err := egress.NewProxy(egress.Config{PrivateOrigins: []string{target.URL, strings.Replace(target.URL, "http://", "https://", 1)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	m, _ := healthyManager(t, upstream.URL)
	c := testHealthClient(t, m, target.URL)
	resp, err := c.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot()[0]; s.InFlight != 1 {
		t.Fatalf("reference released before body: %+v", s)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if s := m.Snapshot()[0]; s.State != StateAvailable || s.ConsecutiveFailures != 0 || s.InFlight != 0 {
		t.Fatalf("target429 poisoned proxy: %+v", s)
	}
	status.Store(http.StatusOK)
	resp, err = c.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	m.replaceProxies(nil)
	if len(m.Snapshot()) != 1 {
		t.Fatal("refresh discarded active response identity")
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(data) != "target body" {
		t.Fatalf("body wrapper changed read semantics: %q %v", data, err)
	}
	if len(m.Snapshot()) != 0 {
		t.Fatal("body completion did not prune retired proxy")
	}
}

type tripFunc func(*http.Request) (*http.Response, error)

func (f tripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTransportPanicAndCancellationReleaseReference(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080")
	tr := WrapTransport(tripFunc(func(*http.Request) (*http.Response, error) { panic("synthetic transport panic") }), m)
	req, _ := http.NewRequest(http.MethodGet, "http://target.invalid", nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected transport panic")
			}
		}()
		_, _ = tr.RoundTrip(req)
	}()
	if s := m.Snapshot()[0]; s.InFlight != 0 || s.State != StateAvailable {
		t.Fatalf("panic leaked or poisoned reference: %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr = WrapTransport(tripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, r.Context().Err()
	}), m)
	_, err := tr.RoundTrip(req.WithContext(ctx))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := m.Snapshot()[0]; s.InFlight != 0 || s.State != StateAvailable {
		t.Fatalf("cancellation leaked or poisoned reference: %+v", s)
	}
}

func TestFixedTransportKeepsSelectedProxy(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080")
	fixed := "http://user:secret@127.0.0.1:8081"
	tr := WrapFixedTransport(tripFunc(func(r *http.Request) (*http.Response, error) {
		selected, ok := SelectedURL(r.Context())
		if !ok || selected != fixed {
			t.Fatalf("selected %q", selected)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}), m, fixed)
	clone := tr.(interface {
		WithBaseTransport(http.RoundTripper) http.RoundTripper
		BaseTransport() http.RoundTripper
	})
	tr = clone.WithBaseTransport(clone.BaseTransport())
	req, _ := http.NewRequestWithContext(core.WithProxyURL(context.Background(), "http://127.0.0.1:8082"), http.MethodGet, "http://target.invalid", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, s := range m.Snapshot() {
		if s.Fixed && s.ID != opaqueID(fixed) {
			t.Fatal("fixed transport inherited request identity")
		}
	}
}
