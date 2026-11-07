package egress

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

type routeKey struct{}
type route struct {
	host string
	ips  []netip.Addr
}
type transport struct {
	policy *Policy
	base   *http.Transport
	proxy  func(*http.Request) (*url.URL, error)
}
type deniedTransport struct{ err error }

func (d deniedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, d.err }

// Client copies c, preserving timeout/jar/redirect policy. Only standard transports
// are supported; custom dialers are replaced so they cannot bypass address checks.
// Environment proxies are not inherited. Explicit Transport.Proxy is trusted as
// an upstream endpoint, but receives pinned target IPs, never target DNS names.
func Client(c *http.Client, cfg Config) *http.Client {
	out := http.Client{}
	if c != nil {
		out = *c
	}
	p, err := New(cfg)
	if err != nil {
		out.Transport = deniedTransport{err}
		return &out
	}
	var base *http.Transport
	if old, ok := out.Transport.(*transport); ok {
		base = safeTransport(old.base)
		base.Proxy = old.proxy
	} else if out.Transport == nil {
		base = safeTransport(nil)
		base.Proxy = nil
	} else if tr, ok := out.Transport.(*http.Transport); ok {
		base = safeTransport(tr)
	} else {
		out.Transport = deniedTransport{ErrTransport}
		return &out
	}
	tr := &transport{policy: p, base: base, proxy: base.Proxy}
	base.Proxy = nil
	base.DialTLS = nil
	base.DialTLSContext = nil
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		r, ok := ctx.Value(routeKey{}).(route)
		host, port, e := net.SplitHostPort(addr)
		if !ok || e != nil || host != r.host {
			return nil, ErrDenied
		}
		return dialIPs(ctx, network, port, r.ips)
	}
	out.Transport = tr
	return &out
}
func dialIPs(ctx context.Context, network, port string, ips []netip.Addr) (net.Conn, error) {
	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var err error
	for _, ip := range ips {
		var c net.Conn
		c, err = d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, ips, err := t.policy.Resolve(req.Context(), req.URL.String())
	if err != nil {
		return nil, err
	}
	r := req.Clone(context.WithValue(req.Context(), routeKey{}, route{u.Hostname(), ips}))
	r.URL = u
	// Host overrides can route through an internal virtual host; use the URL authority.
	if r.Host != "" && r.Host != req.URL.Host {
		return nil, ErrDenied
	}
	r.Host = u.Host
	var upstream *url.URL
	if t.proxy != nil {
		upstream, err = t.proxy(req)
		if err != nil {
			return nil, ErrDenied
		}
	}
	if upstream == nil {
		return t.base.RoundTrip(r)
	}
	if err = ValidateProxy(upstream); err != nil {
		return nil, err
	}
	// A proxy must not resolve the target a second time. TLS still validates the
	// original host and sends its SNI; HTTP preserves the original Host header.
	proxyTr := t.base.Clone()
	proxyTr.Proxy = nil
	proxyTr.DisableKeepAlives = true
	connector := &Proxy{upstream: upstream}
	proxyTr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return connector.dialTunnel(ctx, net.JoinHostPort(ips[0].String(), portOf(u)))
	}
	if proxyTr.TLSClientConfig == nil {
		proxyTr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		proxyTr.TLSClientConfig = proxyTr.TLSClientConfig.Clone()
	}
	proxyTr.TLSClientConfig.ServerName = u.Hostname()
	resp, err := proxyTr.RoundTrip(r)
	if err != nil {
		proxyTr.CloseIdleConnections()
		return nil, err
	}
	resp.Body = &closingBody{ReadCloser: resp.Body, close: proxyTr.CloseIdleConnections}
	return resp, nil
}
func (t *transport) CloseIdleConnections() { t.base.CloseIdleConnections() }

type closingBody struct {
	io.ReadCloser
	close func()
}

func (b *closingBody) Close() error { err := b.ReadCloser.Close(); b.close(); return err }
func ValidateProxy(u *url.URL) error {
	if u == nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return ErrTransport
	}
	switch u.Scheme {
	case "http", "socks5", "socks5h":
		return nil
	default:
		return ErrTransport
	}
}

// Copy only standard transport settings, excluding registered protocol handlers,
// custom TLS/Dial hooks and environment-proxy globals.
func safeTransport(src *http.Transport) *http.Transport {
	if src == nil {
		return &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second}
	}
	dst := &http.Transport{Proxy: src.Proxy, ForceAttemptHTTP2: src.ForceAttemptHTTP2,
		DisableKeepAlives: src.DisableKeepAlives, DisableCompression: src.DisableCompression,
		MaxIdleConns: src.MaxIdleConns, MaxIdleConnsPerHost: src.MaxIdleConnsPerHost, MaxConnsPerHost: src.MaxConnsPerHost,
		IdleConnTimeout: src.IdleConnTimeout, TLSHandshakeTimeout: src.TLSHandshakeTimeout,
		ResponseHeaderTimeout: src.ResponseHeaderTimeout, ExpectContinueTimeout: src.ExpectContinueTimeout,
		MaxResponseHeaderBytes: src.MaxResponseHeaderBytes, WriteBufferSize: src.WriteBufferSize, ReadBufferSize: src.ReadBufferSize}
	if src.TLSClientConfig != nil {
		dst.TLSClientConfig = src.TLSClientConfig.Clone()
	}
	return dst
}
