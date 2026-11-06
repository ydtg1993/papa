package webaccess

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDNSChecksAllAddressesAndPinsConnection(t *testing.T) {
	s, err := New(Config{Policy: Policy{Origins: []Origin{{URL: "https://example.test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var connections int
	s.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
		connections++
		if address != "8.8.8.8:443" {
			t.Errorf("not pinned: %s", address)
		}
		return nil, errors.New("redacted")
	}
	ctx := context.WithValue(context.Background(), dialKey{}, dialPermission{host: "example.test"})
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err = s.dial(ctx, "tcp", "example.test:443"); !errors.Is(err, ErrPolicy) || connections != 0 {
		t.Fatalf("err=%v connections=%d", err, connections)
	}
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	if _, err = s.dial(ctx, "tcp", "example.test:443"); !errors.Is(err, ErrTransport) || connections != 1 {
		t.Fatalf("err=%v connections=%d", err, connections)
	}
	// A subsequent resolution is checked again; no pooled socket can retain
	// an earlier permission after a DNS answer changes to a private address.
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}, nil
	}
	if _, err = s.dial(ctx, "tcp", "example.test:443"); !errors.Is(err, ErrPolicy) || connections != 1 {
		t.Fatalf("err=%v connections=%d", err, connections)
	}
}

func TestEnvironmentProxyIgnoredAndHostPreserved(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	var expectedHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost {
			t.Errorf("Host=%q want %q", r.Host, expectedHost)
		}
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	expectedHost = net.JoinHostPort("example.test", u.Port())
	raw := "http://" + expectedHost
	s, _ := New(localConfig(raw))
	defer s.Close()
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	r, err := s.Get(context.Background(), raw)
	if err != nil || string(r.Body) != "ok" || proxyHits.Load() != 0 {
		t.Fatalf("resp=%+v err=%v proxy=%d", r, err, proxyHits.Load())
	}
}

func TestPolicyIsDetachedAndResourcesCannotBecomePages(t *testing.T) {
	cfg := Config{Policy: Policy{Origins: []Origin{{URL: "https://example.com", PathPrefixes: []string{"/a"}}}, ResourceOrigins: []Origin{{URL: "https://cdn.example.com"}}}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.Policy.Origins[0].PathPrefixes[0] = "/secret"
	if _, _, err = s.target("https://example.com/a", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.target("https://example.com/secret", false); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	if _, _, err = s.target("https://cdn.example.com/script", false); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	if _, _, err = s.target("https://cdn.example.com/script", true); err != nil {
		t.Fatal(err)
	}
}

func TestRobotsDisallowAndCrawlDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\nCrawl-delay: 0.05\n")
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	s, _ := New(localConfig(srv.URL))
	defer s.Close()
	if _, err := s.Get(context.Background(), srv.URL+"/private?q=x"); !errors.Is(err, ErrRobots) {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := s.Get(context.Background(), srv.URL+"/allowed"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("Crawl-delay ignored")
	}
	if s.Usage().Requests != 2 {
		t.Fatal(s.Usage())
	}
}

func TestRequestAndByteBudgetsAndHookFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, strings.Repeat("b", 100))
	}))
	defer srv.Close()
	t.Run("requests", func(t *testing.T) {
		cfg := localConfig(srv.URL)
		cfg.Budgets.MaxRequests = 1
		s, _ := New(cfg)
		defer s.Close()
		if _, err := s.Get(context.Background(), srv.URL); !errors.Is(err, ErrBudget) {
			t.Fatal(err)
		}
		if s.Usage().Requests != 1 {
			t.Fatal(s.Usage())
		}
	})
	t.Run("bytes", func(t *testing.T) {
		cfg := localConfig(srv.URL)
		cfg.Budgets.MaxTotalBytes = 50
		s, _ := New(cfg)
		defer s.Close()
		if r, err := s.Get(context.Background(), srv.URL); !errors.Is(err, ErrBudget) || len(r.Body) != 0 {
			t.Fatalf("%+v %v", r, err)
		}
		if got := s.Usage(); got.Bytes != 50 || got.Requests != 2 {
			t.Fatalf("over-budget body was not charged: %+v", got)
		}
		if _, err := s.Get(context.Background(), srv.URL); !errors.Is(err, ErrBudget) {
			t.Fatal(err)
		}
		if s.Usage().Requests != 2 {
			t.Fatal("new request issued after cumulative exhaustion")
		}
	})
	t.Run("hook", func(t *testing.T) {
		cfg := localConfig(srv.URL)
		cfg.OnBytes = func(context.Context, int64) error { return errors.New("secret") }
		s, _ := New(cfg)
		defer s.Close()
		if r, err := s.Get(context.Background(), srv.URL); !errors.Is(err, ErrAudit) || len(r.Body) != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

func TestTransportErrorsRedactURLAndCloseCancels(t *testing.T) {
	s, _ := New(localConfig("http://127.0.0.1:1"))
	_, err := s.Get(context.Background(), "http://127.0.0.1:1/?token=very-secret")
	if err == nil || strings.Contains(err.Error(), "very-secret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err = s.Get(context.Background(), "http://127.0.0.1:1/"); !errors.Is(err, ErrCanceled) {
		t.Fatal(err)
	}
}

func localConfig(raw string) Config {
	return Config{Policy: Policy{Origins: []Origin{{URL: raw, AllowPrivateNetwork: true}}}}
}

func TestRedirectCannotReachDeniedOrigin(t *testing.T) {
	var hits atomic.Int32
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer denied.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, denied.URL+"/secret?token=private", 302)
	}))
	defer src.Close()
	s, err := New(localConfig(src.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Get(context.Background(), src.URL)
	if !errors.Is(err, ErrPolicy) || hits.Load() != 0 || strings.Contains(err.Error(), "private") {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	if got := s.Usage(); got.Requests != 2 || got.Pages != 1 {
		t.Fatal(got)
	}
}

func TestRobotsFailureBlocksAndCachesOrigin(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(403) }))
	defer srv.Close()
	s, _ := New(localConfig(srv.URL))
	defer s.Close()
	for i := 0; i < 2; i++ {
		if _, err := s.Get(context.Background(), srv.URL); !errors.Is(err, ErrRobots) {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatal(hits.Load())
	}
}

func TestAuditFailurePreventsRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfg := localConfig(srv.URL)
	cfg.OnRequest = func(context.Context) error { return errors.New("secret audit details") }
	s, _ := New(cfg)
	defer s.Close()
	_, err := s.Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrAudit) || hits.Load() != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v hits=%d", err, hits.Load())
	}
}

func TestCompressedBodyBudgetAndIncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		_, _ = z.Write([]byte(strings.Repeat("x", 4096)))
		_ = z.Close()
	}))
	defer srv.Close()
	cfg := localConfig(srv.URL)
	cfg.Budgets.MaxResponseBytes = 512
	s, _ := New(cfg)
	defer s.Close()
	r, err := s.Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrBudget) || len(r.Body) != 0 || s.Usage().Bytes == 0 {
		t.Fatalf("resp=%+v err=%v usage=%+v", r, err, s.Usage())
	}
}

func TestRetryAfterWaitCanBeCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	s, _ := New(localConfig(srv.URL))
	defer s.Close()
	if _, err := s.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.Get(ctx, srv.URL)
	if !errors.Is(err, ErrCanceled) || s.Usage().Requests != 2 {
		t.Fatalf("err=%v usage=%+v", err, s.Usage())
	}
}
