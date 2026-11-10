package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyFailureAttribution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		kind   string
	}{
		{"authentication", 407, "authentication"},
		{"target refused", 403, ""},
		{"target unavailable", 502, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Errorf("method %s", r.Method)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			u.User = url.UserPassword("proxy-user", "proxy-secret")
			p := &Proxy{upstream: u}
			_, err := p.dialTunnel(context.Background(), "127.0.0.1:12345")
			if err == nil {
				t.Fatal("failed tunnel accepted")
			}
			var failure *ProxyFailureError
			classified := errors.As(err, &failure)
			if (tc.kind != "") != classified || classified && failure.ProxyFailure() != tc.kind {
				t.Fatalf("classification %v (%v), expected %q", classified, err, tc.kind)
			}
			if strings.Contains(err.Error(), "proxy-user") || strings.Contains(err.Error(), "proxy-secret") {
				t.Fatal("failure leaks credentials")
			}
		})
	}
}

func TestProxyDialFailureAndCancellationAttribution(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			u, _ := url.Parse(scheme + "://" + addr)
			p := &Proxy{upstream: u}
			_, err := p.dialTunnel(context.Background(), "127.0.0.1:12345")
			var failure *ProxyFailureError
			if !errors.As(err, &failure) || failure.ProxyFailure() != "transport" {
				t.Fatalf("unclassified proxy dial: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = p.dialTunnel(ctx, "127.0.0.1:12345")
			if !errors.Is(err, context.Canceled) || errors.As(err, &failure) {
				t.Fatalf("cancellation poisoned proxy health: %v", err)
			}
		})
	}
}

func TestBrowserGatewayObserverBlocksAndReportsFixedUpstream(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer srv.Close()
	const target = "127.0.0.1:12345"
	gateway, err := NewProxy(Config{PrivateOrigins: []string{"https://" + target}}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	connect := func() {
		t.Helper()
		u, _ := url.Parse(gateway.URL)
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("gateway should report unavailable: %d", resp.StatusCode)
		}
	}
	gateway.SetProxyObserver(func(context.Context) (func(int, error), error) { return nil, errors.New("unavailable") })
	connect()
	if hits.Load() != 0 {
		t.Fatal("observer admission failure contacted upstream")
	}
	result := make(chan error, 1)
	gateway.SetProxyObserver(func(context.Context) (func(int, error), error) {
		return func(status int, err error) { result <- err }, nil
	})
	connect()
	var failure *ProxyFailureError
	if err := <-result; !errors.As(err, &failure) || failure.ProxyFailure() != "authentication" {
		t.Fatalf("gateway lost fixed upstream failure: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("unexpected upstream hits %d", hits.Load())
	}
}
