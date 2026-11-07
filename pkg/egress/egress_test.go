package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

func TestDefaultPolicyAndExplicitPrivateOrigins(t *testing.T) {
	p, _ := New(Config{})
	for _, raw := range []string{"https://0xdocs.example/path", "https://0x123.example"} {
		if _, _, err := Normalize(raw); err != nil {
			t.Fatalf("ordinary domain rejected: %v", err)
		}
	}
	for _, raw := range []string{"http://0x7f000001", "http://0177.0.0.1"} {
		if _, _, err := Normalize(raw); err == nil {
			t.Fatalf("legacy numeric host accepted: %s", raw)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "data:text/plain,secret", "http://user:pass@example.com", "http://127.0.0.1", "http://[::1]", "http://[::ffff:127.0.0.1]", "http://169.254.169.254/latest", "http://10.0.0.1", "http://100.64.0.1", "http://127.1", "http://2130706433", "http://0x7f.0.0.1", "http://host.", "http://[fe80::1%25eth0]", "http://0.0.0.0"} {
		if _, _, err := p.Resolve(context.Background(), raw); err == nil {
			t.Errorf("allowed %q", raw)
		}
	}
	cfg := Config{PrivateOrigins: []string{"http://127.0.0.1:8080"}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PrivateOrigins[0] = "http://127.0.0.1:8081"
	if _, _, err = p.Resolve(context.Background(), "http://127.0.0.1:8080/a?signature=x"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://127.0.0.1:8081", "https://127.0.0.1:8080"} {
		if _, _, err = p.Resolve(context.Background(), raw); err == nil {
			t.Fatal("origin grant expanded")
		}
	}
	for _, raw := range []string{"http://host/path", "http://host?x=1", "http://host#x", "http://*.internal", "http://host:0", "http://host:080"} {
		if _, err := New(Config{PrivateOrigins: []string{raw}}); err == nil {
			t.Errorf("invalid grant %s", raw)
		}
	}
}
func TestDNSMixedAnswerDeniesAndDialIsPinned(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, r.Host) }))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	origin := "http://fixture.test:" + port
	c := Client(nil, Config{PrivateOrigins: []string{origin}})
	defer c.CloseIdleConnections()
	tr := c.Transport.(*transport)
	var queries atomic.Int32
	tr.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		queries.Add(1)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	resp, err := c.Get(origin)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if queries.Load() != 1 || hits.Load() != 1 || string(body) != "fixture.test:"+port {
		t.Fatalf("not pinned: dns=%d hits=%d host=%s", queries.Load(), hits.Load(), body)
	}
	denied := Client(nil, Config{})
	tr = denied.Transport.(*transport)
	tr.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err = denied.Get(origin); !errors.Is(err, ErrDenied) {
		t.Fatalf("mixed answer accepted: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatal("denied destination contacted")
	}
}
func TestRedirectRechecksAndCustomTransportCannotBypass(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	c := Client(source.Client(), Config{PrivateOrigins: []string{source.URL}})
	defer c.CloseIdleConnections()
	if _, err := c.Get(source.URL); !errors.Is(err, ErrDenied) {
		t.Fatalf("redirect err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect touched denied origin")
	}
	c = Client(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { hits.Add(1); return nil, nil })}, Config{PrivateOrigins: []string{source.URL}})
	if _, err := c.Get(source.URL); !errors.Is(err, ErrTransport) {
		t.Fatalf("custom transport err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("custom transport executed")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStandardTransportHooksCannotBypassGuard(t *testing.T) {
	var hits, hooks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		hooks.Add(1)
		return nil, errors.New("custom dial must not run")
	}}
	tr.RegisterProtocol("http", roundTripFunc(func(*http.Request) (*http.Response, error) {
		hooks.Add(1)
		return nil, errors.New("custom protocol must not run")
	}))
	c := Client(&http.Client{Transport: tr}, Config{PrivateOrigins: []string{srv.URL}})
	defer c.CloseIdleConnections()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 1 || hooks.Load() != 0 {
		t.Fatalf("server=%d hooks=%d", hits.Load(), hooks.Load())
	}
}

func TestDNSCancellationPreservesContextError(t *testing.T) {
	p, _ := New(Config{})
	p.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := p.Resolve(ctx, "https://fixture.test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestUpstreamProxyReceivesPinnedIPAndOriginalHost(t *testing.T) {
	var target, host string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host = r.Host; io.WriteString(w, "ok") }))
	defer destination.Close()
	dest, _ := url.Parse(destination.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target = r.Host
		if r.Method != "CONNECT" {
			t.Error("target DNS escaped through proxy")
			http.Error(w, "denied", 403)
			return
		}
		remote, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "unavailable", 502)
			return
		}
		defer remote.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		done := make(chan struct{})
		go func() { io.Copy(remote, rw); remote.Close(); close(done) }()
		io.Copy(client, remote)
		client.Close()
		remote.Close()
		<-done
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	origin := "http://fixture.test:" + dest.Port()
	c := Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}, Config{PrivateOrigins: []string{origin}})
	c.Transport.(*transport).policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	resp, err := c.Get(origin + "/path")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if target != dest.Host || host != "fixture.test:"+dest.Port() {
		t.Fatalf("proxy target=%q host=%q", target, host)
	}
}
func TestBrowserProxyHTTPConnectAndClose(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secured") }))
	defer srv.Close()
	gateway, err := NewProxy(Config{PrivateOrigins: []string{srv.URL}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	u, _ := url.Parse(gateway.URL)
	c := srv.Client()
	tr := c.Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	c = &http.Client{Transport: tr, Timeout: 3 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secured" {
		t.Fatalf("bad response %s", body)
	}
	c.CloseIdleConnections()
	// The raw CONNECT tunnel must be torn down by gateway.Close, too.
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dst, _ := url.Parse(srv.URL)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dst.Host, dst.Host)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT failed: %v", err)
	}
	gateway.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = reader.ReadByte(); err == nil {
		t.Fatal("tunnel survived close")
	}
	denied, _ := NewProxy(Config{}, "")
	defer denied.Close()
	du, _ := url.Parse(denied.URL)
	c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(du)}, Timeout: time.Second}
	defer c.CloseIdleConnections()
	if _, err = c.Get(srv.URL); err == nil {
		t.Fatal("default proxy allowed private CONNECT")
	}
	resp, err = c.Get(strings.Replace(srv.URL, "https:", "http:", 1))
	if err == nil {
		resp.Body.Close()
		t.Fatal("private HTTP returned a fabricated document")
	}
}
