package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	socks "golang.org/x/net/proxy"
)

// Proxy is a loopback browser gateway, owned by one browser instance. Close also
// cancels pending requests and closes hijacked CONNECT tunnels.
type Proxy struct {
	URL      string
	srv      *http.Server
	cancel   context.CancelFunc
	mu       sync.Mutex
	conns    map[net.Conn]bool
	closed   bool
	policy   *Policy
	upstream *url.URL
	client   *http.Client
	observer func(context.Context) (func(int, error), error)
}

// SetProxyObserver installs optional upstream health admission/feedback. The
// observer must acquire the already fixed upstream, never rotate it. CONNECT
// success reports proxy tunnel establishment; encrypted target HTTP statuses are
// not observable here. Ordinary HTTP reports after its response body finishes.
func (p *Proxy) SetProxyObserver(observer func(context.Context) (func(int, error), error)) {
	p.mu.Lock()
	p.observer = observer
	p.mu.Unlock()
}

func (p *Proxy) observeProxy(ctx context.Context) (func(int, error), error) {
	p.mu.Lock()
	observer := p.observer
	p.mu.Unlock()
	if observer == nil || p.upstream == nil {
		return func(int, error) {}, nil
	}
	return observer(ctx)
}

func NewProxy(cfg Config, upstream string) (*Proxy, error) {
	policy, err := New(cfg)
	if err != nil {
		return nil, err
	}
	p := &Proxy{policy: policy, conns: map[net.Conn]bool{}}
	tr := safeTransport(nil)
	if upstream != "" {
		u, e := url.Parse(upstream)
		if e != nil || ValidateProxy(u) != nil {
			return nil, ErrTransport
		}
		p.upstream = u
		tr.Proxy = http.ProxyURL(u)
	}
	p.client = Client(&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	p.URL = "http://" + ln.Addr().String()
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	_ = p.srv.Close()
	p.client.CloseIdleConnections()
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "egress: HTTP upgrades unsupported", http.StatusForbidden)
		return
	}
	req := r.Clone(r.Context())
	req.RequestURI = ""
	stripHop(req.Header)
	finish, err := p.observeProxy(r.Context())
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	status := 0
	defer func() { finish(status, err) }()
	resp, err := p.client.Do(req)
	if err != nil {
		// Do not fabricate a document that a crawler could store as source evidence.
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
}
func stripHop(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, k := range strings.Split(line, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host != r.Host || r.URL.Path != "" || r.URL.RawQuery != "" {
		http.Error(w, "egress: denied", 403)
		return
	}
	u, ips, err := p.policy.Resolve(r.Context(), "https://"+r.Host)
	if err != nil {
		http.Error(w, "egress: denied", 403)
		return
	}
	target := net.JoinHostPort(ips[0].String(), portOf(u))
	finish, err := p.observeProxy(r.Context())
	if err != nil {
		http.Error(w, "egress: proxy unavailable", http.StatusBadGateway)
		return
	}
	upstream, err := p.dialTunnel(r.Context(), target)
	if err != nil {
		finish(0, err)
		http.Error(w, "egress: destination unavailable", 502)
		return
	}
	finish(http.StatusOK, nil)
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "egress: unsupported server", 500)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		client.Close()
		upstream.Close()
		return
	}
	p.conns[client] = true
	p.conns[upstream] = true
	p.mu.Unlock()
	defer func() {
		client.Close()
		upstream.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, upstream)
		p.mu.Unlock()
	}()
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if rw.Flush() != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, rw); upstream.Close(); close(done) }()
	_, _ = io.Copy(client, upstream)
	client.Close()
	upstream.Close()
	<-done
}
func (p *Proxy) dialTunnel(ctx context.Context, target string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if p.upstream == nil {
		return d.DialContext(ctx, "tcp", target)
	}
	if p.upstream.Scheme == "socks5" || p.upstream.Scheme == "socks5h" {
		proxy, err := socks.FromURL(p.upstream, proxyDialer{base: d})
		if err != nil {
			return nil, ErrTransport
		}
		cd, ok := proxy.(socks.ContextDialer)
		if !ok {
			return nil, ErrTransport
		}
		return cd.DialContext(ctx, "tcp", target)
	}
	addr := p.upstream.Host
	if p.upstream.Port() == "" {
		addr = net.JoinHostPort(p.upstream.Hostname(), "80")
	}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, proxyFailure(ctx, "transport", err)
	}
	success := false
	defer func() {
		if !success {
			c.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if p.upstream.User != nil {
		pass, _ := p.upstream.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.upstream.User.Username()+":"+pass)))
	}
	if err = req.Write(c); err != nil {
		return nil, proxyFailure(ctx, "transport", err)
	}
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, proxyFailure(ctx, "transport", err)
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return nil, proxyFailure(ctx, "authentication", ErrDenied)
	}
	if resp.StatusCode != 200 {
		return nil, ErrDenied
	}
	_ = c.SetDeadline(time.Time{})
	success = true
	return &bufferedConn{Conn: c, r: reader}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }
