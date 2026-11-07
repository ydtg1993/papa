package webaccess

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

type Config struct {
	Policy   Policy
	Budgets  Budgets
	Renderer RendererConfig
	// Hooks must be concurrency-safe. Their errors are redacted. OnRequest
	// runs before DNS/network I/O; OnBytes runs before retaining each chunk.
	OnRequest func(context.Context) error
	OnBytes   func(context.Context, int64) error
}

type RendererConfig struct {
	Enabled           bool
	ExecutablePath    string
	TempDir           string
	GlobalConcurrency int
}
type RenderOptions struct {
	ObserveDuration time.Duration
	RecordAPI       bool
	// Wait replaces ObserveDuration; the two cannot be specified together.
	Wait *RenderWait
}
type Response struct {
	URL         string
	StatusCode  int
	ContentType string
	Body        []byte
	Rendered    bool
}

// Bytes conservatively counts wire bodies plus decompressed copies (when
// compressed), and rendered DOM copies. Failed requests remain charged.
type Usage struct {
	Requests int
	Pages    int
	Bytes    int64
}

type Session struct {
	origins, resources map[string]allowedOrigin
	budget             Budgets
	renderer           RendererConfig
	onRequest          func(context.Context) error
	onBytes            func(context.Context, int64) error
	minInterval        time.Duration
	ctx                context.Context
	cancel             context.CancelFunc
	transport          *http.Transport
	lookup             func(context.Context, string, string) ([]netip.Addr, error)
	connect            func(context.Context, string, string) (net.Conn, error)
	mu                 sync.Mutex
	accountMu          sync.Mutex
	usage              Usage
	states             map[string]*originState
	renderGate         chan struct{}
}

func New(cfg Config) (*Session, error) {
	origins, err := compileOrigins(cfg.Policy.Origins)
	if err != nil || len(origins) == 0 {
		return nil, ErrPolicy
	}
	resources, err := compileOrigins(cfg.Policy.ResourceOrigins)
	if err != nil {
		return nil, err
	}
	// The same origin cannot gain a broader network permission by changing
	// from a document to a resource request.
	for k, a := range resources {
		if b, ok := origins[k]; ok && a.private != b.private {
			return nil, ErrPolicy
		}
	}
	b, err := defaults(cfg.Budgets)
	if err != nil {
		return nil, err
	}
	if cfg.Policy.MinInterval < 0 || cfg.Policy.MinInterval > 600*time.Second {
		return nil, ErrPolicy
	}
	if cfg.Renderer.GlobalConcurrency == 0 {
		cfg.Renderer.GlobalConcurrency = 1
	}
	if cfg.Renderer.GlobalConcurrency < 1 || cfg.Renderer.GlobalConcurrency > 4 {
		return nil, ErrBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.MaxDuration)
	s := &Session{origins: origins, resources: resources, budget: b, renderer: cfg.Renderer, onRequest: cfg.OnRequest, onBytes: cfg.OnBytes, minInterval: cfg.Policy.MinInterval, ctx: ctx, cancel: cancel, states: map[string]*originState{}, lookup: net.DefaultResolver.LookupNetIP, renderGate: make(chan struct{}, 1)}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	s.connect = dialer.DialContext
	s.transport = &http.Transport{Proxy: nil, DialContext: s.dial, DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	return s, nil
}

func (s *Session) Close() error { s.cancel(); s.transport.CloseIdleConnections(); return nil }
func (s *Session) Usage() Usage { s.mu.Lock(); defer s.mu.Unlock(); return s.usage }

// Budgets returns a value copy of the validated limits, including defaults,
// so hosts can persist the actual run limits without duplicating this policy.
func (s *Session) Budgets() Budgets { return s.budget }

func (s *Session) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	return c, func() { stop(); cancel() }
}

func (s *Session) page(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return ErrCanceled
	}
	if s.usage.Pages >= s.budget.MaxPages {
		return ErrBudget
	}
	s.usage.Pages++
	return nil
}
func (s *Session) request(ctx context.Context) error {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if ctx.Err() != nil {
		return ErrCanceled
	}
	usage := s.Usage()
	if usage.Requests >= s.budget.MaxRequests || usage.Bytes >= s.budget.MaxTotalBytes {
		return ErrBudget
	}
	if s.onRequest != nil {
		if err := s.onRequest(ctx); err != nil {
			return ErrAudit
		}
	}
	s.mu.Lock()
	s.usage.Requests++
	s.mu.Unlock()
	return nil
}
func (s *Session) bytes(ctx context.Context, n int64) error {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	return s.accountBytes(ctx, n)
}

// accountBytes is called with accountMu held. Charge an exhausted budget up
// to its cap even when rejecting the current chunk; failed work is not free.
func (s *Session) accountBytes(ctx context.Context, n int64) error {
	if ctx.Err() != nil {
		return ErrCanceled
	}
	if n < 0 {
		return ErrBudget
	}
	remaining := s.budget.MaxTotalBytes - s.Usage().Bytes
	exceeded := n > remaining
	if exceeded {
		n = remaining
	}
	if s.onBytes != nil {
		if n > 0 {
			if err := s.onBytes(ctx, n); err != nil {
				return ErrAudit
			}
		}
	}
	s.mu.Lock()
	s.usage.Bytes += n
	s.mu.Unlock()
	if exceeded {
		return ErrBudget
	}
	return nil
}

// readChunk serializes body accounting with other readers/DOM copies. At
// most one overflow byte is consumed to distinguish EOF from an oversized
// stream; no caller can repeatedly fetch uncharged over-budget chunks.
func (s *Session) readChunk(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}
	remaining := s.budget.MaxTotalBytes - s.Usage().Bytes
	if int64(len(buf)) > remaining+1 {
		buf = buf[:remaining+1]
	}
	n, err := r.Read(buf)
	if n > 0 {
		if chargeErr := s.accountBytes(ctx, int64(n)); chargeErr != nil {
			return 0, chargeErr
		}
	}
	return n, err
}

type dialPermission struct {
	host    string
	private bool
}
type dialKey struct{}

func (s *Session) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrPolicy
	}
	permission, ok := ctx.Value(dialKey{}).(dialPermission)
	if !ok || host != permission.host {
		return nil, ErrPolicy
	}
	ips, err := s.lookup(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrTransport
	}
	for _, ip := range ips {
		if !ip.IsValid() || ip.Zone() != "" || !permission.private && !publicIP(ip) {
			return nil, ErrPolicy
		}
	}
	for _, ip := range ips {
		conn, err := s.connect(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ErrCanceled
		}
	}
	return nil, ErrTransport
}
