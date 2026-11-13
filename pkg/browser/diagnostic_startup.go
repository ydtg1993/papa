package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

// NewDiagnosticPool uses the ordinary managed page initializer, with a bounded
// process startup path. Rod v0.116.2's Launcher contains unbounded HTTP/guard
// waits, so diagnostics must not use it to launch. Leakless is rejected rather
// than silently changing the selected environment's process semantics.
func NewDiagnosticPool(cfg PoolConfig) (*Pool, error) {
	if cfg.Leakless {
		return nil, fmt.Errorf("bounded identity diagnostics do not support leakless=true; selected process configuration is unverified")
	}
	if cfg.ProxyManager != nil {
		return nil, fmt.Errorf("diagnostic pool must not use production proxy state")
	}
	if len(cfg.Network.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("diagnostic pool requires an explicit loopback origin allowlist")
	}
	for _, raw := range cfg.Network.AllowedOrigins {
		u, err := url.Parse(raw)
		if err != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			return nil, fmt.Errorf("diagnostic pool permits loopback origins only")
		}
	}
	cfg.Network.PrivateOrigins = append([]string(nil), cfg.Network.AllowedOrigins...)
	for key := range cfg.Flags {
		k := strings.TrimLeft(key, "-")
		if strings.HasPrefix(k, "rod-") || k == "" || k == "proxy-server" || k == "user-data-dir" || k == "disk-cache-dir" || k == "load-extension" || k == "remote-debugging-port" || k == "remote-debugging-address" {
			return nil, fmt.Errorf("bounded diagnostic startup does not support flag %s", k)
		}
	}
	cfg.DefaultCookies = nil
	headers := map[string]string{}
	for key, value := range cfg.DefaultHeaders {
		if protectedIdentityHeader(key) {
			headers[key] = value
		}
	}
	cfg.DefaultHeaders = headers
	cfg.diagnosticStartup = true
	return NewPool(cfg)
}

var diagnosticDownloadSlot = make(chan struct{}, 1)

// Resolve the managed binary without Rod's unbounded validation subprocess or
// process-wide LockPort. Download itself receives the operation context.
func diagnosticBinary(ctx context.Context, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	select {
	case diagnosticDownloadSlot <- struct{}{}:
		defer func() { <-diagnosticDownloadSlot }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	download := launcher.NewBrowser()
	download.Context = ctx
	if _, err := os.Stat(download.BinPath()); err == nil {
		return download.BinPath(), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := download.Download(); err != nil {
		return "", err
	}
	return download.BinPath(), nil
}

type diagnosticOutput struct {
	mu   sync.Mutex
	tail string
	url  chan string
}

var diagnosticWS = regexp.MustCompile(`ws://[^\s]+\s`)

func (o *diagnosticOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail += string(p)
	if len(o.tail) > 64<<10 {
		o.tail = o.tail[len(o.tail)-(64<<10):]
	}
	if raw := diagnosticWS.FindString(o.tail); raw != "" {
		select {
		case o.url <- strings.TrimSpace(raw):
		default:
		}
	}
	return len(p), nil
}

type diagnosticProcess struct {
	cmd         *exec.Cmd
	done        chan struct{}
	dir         string
	ws          *cdp.WebSocket
	once        sync.Once
	err         error
	stopContext atomic.Pointer[func() bool]
}

func (p *diagnosticProcess) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), browserCleanupTimeout)
	defer cancel()
	return p.closeWithin(ctx)
}

func (p *diagnosticProcess) closeWithin(ctx context.Context) error {
	p.once.Do(func() {
		if stop := p.stopContext.Load(); stop != nil {
			(*stop)()
		}
		if p.ws != nil {
			_ = p.ws.Close()
		}
		if p.cmd != nil && p.cmd.Process != nil {
			stopDiagnosticProcess(ctx, p.cmd.Process)
			select {
			case <-p.done:
			case <-ctx.Done():
				p.err = fmt.Errorf("diagnostic browser process did not exit within cleanup timeout: %w", ctx.Err())
			}
		}
		if p.dir != "" {
			p.err = errors.Join(p.err, removeDiagnosticProfile(ctx, p.dir))
		}
	})
	return p.err
}

// CloseDiagnostic closes only an owned diagnostic instance and reports its
// bounded process/profile cleanup result. Ordinary Close keeps its existing API.
// Calling this before returning a WithBrowser callback is safe: Put observes the
// closed instance and releases its pool slot through the usual dead-browser path.
func (b *Browser) CloseDiagnostic() error {
	if b == nil || b.diagnosticProcess == nil {
		return errors.New("browser is not an owned diagnostic instance")
	}
	b.Close()
	return b.diagnosticProcess.close()
}

// This dialer bounds both the TCP dial and Rod's otherwise unbounded WebSocket
// handshake read. Cancellation actively closes the owned socket.
type diagnosticDialer struct {
	ctx  context.Context
	conn net.Conn
	stop func() bool
}

func (d *diagnosticDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	d.conn = c
	if deadline, ok := d.ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	d.stop = context.AfterFunc(d.ctx, func() { _ = c.Close() })
	return c, nil
}
func (d *diagnosticDialer) release() error {
	if d.stop != nil {
		d.stop()
	}
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if d.conn != nil {
		return d.conn.SetDeadline(time.Time{})
	}
	return nil
}

type diagnosticCDP struct {
	client  *cdp.Client
	startup context.Context
	ready   atomic.Bool
}

func (c *diagnosticCDP) Event() <-chan *cdp.Event { return c.client.Event() }
func (c *diagnosticCDP) Call(ctx context.Context, session, method string, params interface{}) ([]byte, error) {
	if !c.ready.Load() {
		ctx = c.startup
	}
	return c.client.Call(ctx, session, method, params)
}

func (p *Pool) newDiagnosticBrowser(ctx context.Context, useProxy bool) (result *Browser, err error) {
	taskCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	l := p.newLauncher(false)
	upstream, observe, state, err := p.selectBrowserProxy(useProxy, l.Get(flags.Flag("proxy-server")))
	if err != nil {
		return nil, err
	}
	gateway, err := egress.NewProxy(p.cfg.Network, upstream)
	if err != nil {
		return nil, err
	}
	if observe != nil {
		gateway.SetProxyObserver(observe)
	}
	defer func() {
		if err != nil {
			gateway.Close()
		}
	}()
	secureLauncher(l, gateway.URL)
	dir, err := NewTempProfile(l, p.cfg.TempDir, "browser-diagnostic-")
	if err != nil {
		return nil, err
	}
	process := &diagnosticProcess{dir: dir}
	defer func() {
		if err != nil {
			err = errors.Join(err, process.close())
		}
	}()
	bin, err := diagnosticBinary(ctx, p.cfg.BrowserPath)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	output := &diagnosticOutput{url: make(chan string, 1)}
	cmd := exec.Command(bin, l.FormatArgs()...)
	configureDiagnosticProcess(cmd)
	cmd.Env, _ = l.GetFlags(flags.Env)
	cmd.Dir = l.Get(flags.WorkingDir)
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	process.cmd = cmd
	process.done = make(chan struct{})
	go func() { _ = cmd.Wait(); close(process.done) }()
	var controlURL string
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-process.done:
		return nil, fmt.Errorf("diagnostic browser exited before announcing a control endpoint")
	case controlURL = <-output.url:
	}
	u, parseErr := url.Parse(controlURL)
	if parseErr != nil || u.Scheme != "ws" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil {
		return nil, fmt.Errorf("diagnostic browser announced an invalid control endpoint")
	}
	dialer := &diagnosticDialer{ctx: ctx}
	ws := &cdp.WebSocket{Dialer: dialer}
	if err = ws.Connect(ctx, controlURL, nil); err != nil {
		if dialer.conn != nil {
			_ = dialer.conn.Close()
		}
		if dialer.stop != nil {
			dialer.stop()
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("connect diagnostic browser: %w", err)
	}
	process.ws = ws
	client := &diagnosticCDP{client: cdp.New().Start(ws), startup: ctx}
	controller := rod.New().Client(client)
	if err = controller.Connect(); err != nil {
		return nil, fmt.Errorf("initialize diagnostic CDP: %w", err)
	}
	if err = dialer.release(); err != nil {
		return nil, err
	}
	client.ready.Store(true)
	stop := context.AfterFunc(taskCtx, func() { _ = process.close() })
	process.stopContext.Store(&stop)
	if err = taskCtx.Err(); err != nil {
		return nil, err
	}
	return &Browser{Browser: controller, diagnosticProcess: process, gateway: gateway, proxyState: state, useProxy: useProxy, defaultDevice: p.cfg.DefaultDevice, defaultCookies: copyCookies(p.cfg.DefaultCookies)}, nil
}
