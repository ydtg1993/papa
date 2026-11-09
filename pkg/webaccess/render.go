package webaccess

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// Every active renderer's limit applies to the shared process count. A caller
// with a lower limit cannot be crowded out by a caller configured for four.
var browsers = struct {
	sync.Mutex
	limits  map[*Session]int
	changed chan struct{}
}{limits: map[*Session]int{}, changed: make(chan struct{})}

func acquireBrowser(ctx context.Context, s *Session) (func(), error) {
	for {
		browsers.Lock()
		limit := s.renderer.GlobalConcurrency
		for _, n := range browsers.limits {
			if n < limit {
				limit = n
			}
		}
		if len(browsers.limits) < limit {
			browsers.limits[s] = s.renderer.GlobalConcurrency
			browsers.Unlock()
			return func() {
				browsers.Lock()
				delete(browsers.limits, s)
				close(browsers.changed)
				browsers.changed = make(chan struct{})
				browsers.Unlock()
			}, nil
		}
		changed := browsers.changed
		browsers.Unlock()
		select {
		case <-ctx.Done():
			return nil, ErrCanceled
		case <-changed:
		}
	}
}

// denyProxy never dials a destination, handles CONNECT exactly like other
// methods, and is the fail-closed outlet for targets not covered by page Fetch
// interception (popups, workers, websocket handshakes, browser services).
func denyProxy() (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, ErrRendererUnavailable
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusForbidden)
	}), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

func guardedLauncher(ctx context.Context, bin, profile, proxy string) *launcher.Launcher {
	l := launcher.New().Context(ctx).Bin(bin).Headless(true).NoSandbox(false).Leakless(false).RemoteDebuggingPort(0).UserDataDir(profile).Proxy(proxy)
	// Rod's general automation defaults weaken isolation and disable sandbox
	// inside containers. Controlled access retains Chromium's protections;
	// unsupported hosts fail explicitly rather than launching unsandboxed.
	l.Delete(flags.Flag("disable-site-isolation-trials"))
	l.Delete(flags.Flag("disable-ipc-flooding-protection"))
	l.Delete(flags.Flag("enable-features"))
	// Chromium bypasses loopback proxies by default. <-loopback> explicitly
	// removes that exception. Wildcard DNS denial and disabling QUIC/WebRTC
	// direct UDP prevent secondary network stacks escaping the HTTP proxy.
	l.Set(flags.Flag("proxy-bypass-list"), "<-loopback>")
	l.Set(flags.Flag("host-resolver-rules"), "MAP * ~NOTFOUND")
	l.Set(flags.Flag("disable-quic"))
	l.Set(flags.Flag("force-webrtc-ip-handling-policy"), "disable_non_proxied_udp")
	l.Set(flags.Flag("webrtc-ip-handling-policy"), "disable_non_proxied_udp")
	l.Set(flags.Flag("disable-background-networking"))
	l.Set(flags.Flag("disable-extensions"))
	l.Set(flags.Flag("disable-component-update"))
	l.Set(flags.Flag("disable-sync"))
	l.Set(flags.Flag("no-first-run"))
	l.Set(flags.Flag("no-default-browser-check"))
	l.Set(flags.Flag("disable-features"), "MediaRouter,OptimizationHints,DnsOverHttps,WebTransport,DirectSockets,SpeculationRulesPrefetch,Prerender2")
	return l
}

func (s *Session) Render(ctx context.Context, raw string, opts RenderOptions) (Response, []Response, error) {
	return s.render(ctx, raw, opts, nil)
}

func (s *Session) render(ctx context.Context, raw string, opts RenderOptions, walk *browserWalk) (Response, []Response, error) {
	if !s.renderer.Enabled {
		return Response{}, nil, ErrRendererUnavailable
	}
	if opts.Wait != nil {
		if opts.ObserveDuration != 0 || ValidateRenderWait(*opts.Wait) != nil {
			return Response{}, nil, ErrWaitInvalid
		}
		// Detach caller-owned options for the lifetime of this render.
		wait := *opts.Wait
		opts.Wait = &wait
	}
	if opts.ObserveDuration < 0 || opts.ObserveDuration > 30*time.Second {
		return Response{}, nil, ErrBudget
	}
	if opts.ObserveDuration == 0 && opts.Wait == nil {
		opts.ObserveDuration = 500 * time.Millisecond
	}
	ctx, cancel := s.operation(ctx)
	defer cancel()
	ctx, stopRender := context.WithCancelCause(ctx)
	defer stopRender(nil)
	if _, _, err := s.target(raw, false); err != nil {
		return Response{}, nil, err
	}
	if err := s.page(ctx); err != nil {
		return Response{}, nil, err
	}
	select {
	case s.renderGate <- struct{}{}:
		defer func() { <-s.renderGate }()
	case <-ctx.Done():
		return Response{}, nil, ErrCanceled
	}
	release, err := acquireBrowser(ctx, s)
	if err != nil {
		return Response{}, nil, err
	}
	defer release()
	bin := s.renderer.ExecutablePath
	if bin == "" {
		var ok bool
		bin, ok = launcher.LookPath()
		if !ok {
			return Response{}, nil, ErrRendererUnavailable
		}
	}
	profile, err := os.MkdirTemp(s.renderer.TempDir, "papa-webaccess-")
	if err != nil {
		return Response{}, nil, ErrRendererUnavailable
	}
	defer os.RemoveAll(profile)
	proxy, closeProxy, err := denyProxy()
	if err != nil {
		return Response{}, nil, err
	}
	defer closeProxy()
	l := guardedLauncher(ctx, bin, profile, proxy)
	defer func() {
		if l.PID() != 0 {
			l.Kill()
			done := make(chan struct{})
			go func() { l.Cleanup(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}()
	control, err := l.Launch()
	if err != nil {
		return Response{}, nil, renderError(ctx, err)
	}
	b := rod.New().Context(ctx).ControlURL(control)
	if err = b.Connect(); err != nil {
		return Response{}, nil, renderError(ctx, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = b.Context(cleanup).Close()
	}()
	if err = (proto.BrowserSetDownloadBehavior{Behavior: proto.BrowserSetDownloadBehaviorBehaviorDeny}).Call(b); err != nil {
		return Response{}, nil, ErrRendererUnavailable
	}
	page, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return Response{}, nil, renderError(ctx, err)
	}
	if err = (proto.NetworkSetBypassServiceWorker{Bypass: true}).Call(page); err != nil {
		return Response{}, nil, ErrRendererUnavailable
	}
	frames, err := (proto.PageGetFrameTree{}).Call(page)
	if err != nil {
		return Response{}, nil, ErrRendererUnavailable
	}
	rootFrame := frames.FrameTree.Frame.ID
	if err = (proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{{URLPattern: "*", RequestStage: proto.FetchRequestStageRequest}}}).Call(page); err != nil {
		return Response{}, nil, ErrRendererUnavailable
	}
	var mu sync.Mutex
	var firstErr error
	var top Response
	var records []Response
	redirects := map[proto.FetchRequestID]int{}
	topRequests := 0
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		// A rejected resource aborts this render, preventing a hostile page
		// from growing an unbounded queue of already-rejected requests.
		stopRender(err)
	}
	// EachEvent dispatch is sequential; handlers intentionally keep network
	// requests serial here, bounding the number of paused body allocations.
	eventCtx, stopEvents := context.WithCancel(ctx)
	defer stopEvents()
	wait := page.Context(eventCtx).EachEvent(func(e *proto.FetchRequestPaused) {
		fail := func(err error) {
			setErr(err)
			_ = (proto.FetchFailRequest{RequestID: e.RequestID, ErrorReason: proto.NetworkErrorReasonBlockedByClient}).Call(page)
		}
		if ctx.Err() != nil {
			fail(ErrCanceled)
			return
		}
		if e.Request.Method != http.MethodGet {
			fail(ErrPolicy)
			return
		}
		isTop := e.FrameID == rootFrame && e.ResourceType == proto.NetworkResourceTypeDocument
		if isTop && e.RedirectedRequestID == "" {
			if topRequests > 0 {
				if err := s.page(eventCtx); err != nil {
					fail(err)
					return
				}
			}
			topRequests++
		}
		hops := 0
		if e.RedirectedRequestID != "" {
			hops = redirects[e.RedirectedRequestID] + 1
		}
		redirects[e.RequestID] = hops
		if hops > s.budget.MaxRedirects {
			fail(ErrBudget)
			return
		}
		r, headers, err := s.fetch(eventCtx, e.Request.URL, !isTop)
		if err != nil {
			// Snapshot completion deliberately stops unfinished resource fetches.
			// This internal cancellation is not a transport failure. Keep actual
			// policy/budget/audit errors and outer cancellation fail-closed; retain
			// all charges already incurred and never record an incomplete body.
			if errors.Is(err, ErrCanceled) && eventCtx.Err() != nil && ctx.Err() == nil {
				return
			}
			if errors.Is(err, ErrCanceled) && ctx.Err() == nil {
				err = ErrTransport
			}
			fail(err)
			return
		}
		responseHeaders := []*proto.FetchHeaderEntry{}
		for _, name := range []string{"Content-Type", "Content-Security-Policy", "X-Content-Type-Options", "Access-Control-Allow-Origin"} {
			if v := headers.Get(name); v != "" {
				responseHeaders = append(responseHeaders, &proto.FetchHeaderEntry{Name: name, Value: v})
			}
		}
		if redirect(r.StatusCode) {
			u, _, _ := s.target(r.URL, !isTop)
			next, err := u.Parse(headers.Get("Location"))
			if err != nil || headers.Get("Location") == "" {
				fail(ErrTransport)
				return
			}
			if _, _, err = s.target(next.String(), !isTop); err != nil {
				fail(err)
				return
			}
			responseHeaders = append(responseHeaders, &proto.FetchHeaderEntry{Name: "Location", Value: next.String()})
		}
		responseHeaders = append(responseHeaders, &proto.FetchHeaderEntry{Name: "Cache-Control", Value: "no-store"})
		// CDP expects a base64 string, including "" for an empty response.
		// Rod serializes nil []byte as null, which Chromium rejects. Keep the
		// normalization at this boundary; source evidence/accounting is unchanged.
		body := r.Body
		if body == nil {
			body = []byte{}
		}
		if err = (proto.FetchFulfillRequest{RequestID: e.RequestID, ResponseCode: r.StatusCode, ResponseHeaders: responseHeaders, Body: body}).Call(page); err != nil {
			// Navigation or page script can cancel a paused resource while the
			// host is fetching it. It was already authorized and charged, but
			// must not be recorded as delivered evidence. Never suppress a
			// document failure or any preceding policy/budget/audit error.
			if !isTop && canceledInterception(err) {
				return
			}
			fail(renderError(ctx, err))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if isTop && !redirect(r.StatusCode) {
			top = r
		}
		mediaType, _, _ := mime.ParseMediaType(r.ContentType)
		if opts.RecordAPI && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")) {
			records = append(records, r)
		}
	})
	eventsDone := make(chan struct{})
	go func() { defer close(eventsDone); wait() }()
	defer func() { stopEvents(); <-eventsDone }()
	if err = page.Navigate(raw); err != nil {
		mu.Lock()
		e := firstErr
		mu.Unlock()
		if e != nil {
			return Response{}, nil, e
		}
		return Response{}, nil, renderError(ctx, err)
	}
	if err = page.WaitLoad(); err != nil {
		return Response{}, nil, renderError(ctx, err)
	}
	if opts.Wait != nil {
		if err = waitRenderDOM(ctx, page, rootFrame, *opts.Wait); err != nil {
			return Response{}, nil, err
		}
	} else {
		if err = waitUntil(ctx, time.Now().Add(opts.ObserveDuration)); err != nil {
			return Response{}, nil, renderError(ctx, err)
		}
	}
	if walk != nil {
		snapshot := func() (Response, error) {
			mu.Lock()
			result := top
			saved := firstErr
			mu.Unlock()
			if saved != nil {
				return Response{}, saved
			}
			// Read same-document history changes from the isolated world too.
			var actual string
			if err := evalBrowser(ctx, page, rootFrame, `location.href`, &actual); err != nil {
				return Response{}, err
			}
			u, _, err := s.target(actual, false)
			if err != nil {
				return Response{}, err
			}
			result.URL = u.String()
			return s.captureDOM(ctx, page, rootFrame, result)
		}
		if err := s.walkBrowser(ctx, page, rootFrame, walk, snapshot); err != nil {
			return Response{}, nil, err
		}
	}
	// Freeze outbound work before extracting the bounded DOM and returning
	// usage. No handler may continue charging requests after Render returns.
	stopEvents()
	<-eventsDone
	mu.Lock()
	savedErr := firstErr
	result := top
	captured := append([]Response(nil), records...)
	mu.Unlock()
	if savedErr != nil {
		return Response{}, nil, savedErr
	}
	if result.URL == "" {
		return Response{}, nil, ErrTransport
	}
	if walk != nil {
		return result, captured, nil
	}
	result, err = s.captureDOM(ctx, page, rootFrame, result)
	return result, captured, err
}

func (s *Session) captureDOM(ctx context.Context, page *rod.Page, rootFrame proto.PageFrameID, result Response) (Response, error) {
	// Fixed extraction code in an isolated world: page scripts cannot replace
	// DOM getters or TextEncoder to forge evidence or evade the CDP byte cap.
	// No model-supplied code is accepted here.
	world, err := (proto.PageCreateIsolatedWorld{FrameID: rootFrame, WorldName: "papa-bounded-dom"}).Call(page)
	if err != nil {
		return Response{}, renderError(ctx, err)
	}
	expression := fmt.Sprintf(`(() => { const s = document.documentElement.outerHTML; const limit = %d; return s.length > limit || new TextEncoder().encode(s).byteLength > limit ? null : s })()`, s.budget.MaxResponseBytes)
	dom, err := (proto.RuntimeEvaluate{Expression: expression, ContextID: world.ExecutionContextID, ReturnByValue: true, Silent: true}).Call(page)
	if err != nil {
		return Response{}, renderError(ctx, err)
	}
	if dom.ExceptionDetails != nil || dom.Result == nil {
		return Response{}, ErrTransport
	}
	if dom.Result.Value.Nil() {
		return Response{}, ErrBudget
	}
	body := []byte(dom.Result.Value.Str())
	if int64(len(body)) > s.budget.MaxResponseBytes {
		return Response{}, ErrBudget
	}
	if err = s.bytes(ctx, int64(len(body))); err != nil {
		return Response{}, err
	}
	result.Body = body
	result.Rendered = true
	// This is a DOM snapshot, not the original HTTP entity. Existing rule
	// sample/verification contracts intentionally use unknown status for DOM.
	result.StatusCode = 0
	result.ContentType = "text/html; charset=utf-8"
	return result, nil
}

func renderError(ctx context.Context, err error) error {
	for _, known := range []error{ErrPolicy, ErrBudget, ErrRobots, ErrTransport, ErrAudit, ErrRendererUnavailable} {
		if errors.Is(context.Cause(ctx), known) {
			return known
		}
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrCanceled
	}
	return ErrRendererUnavailable
}

func canceledInterception(err error) bool {
	var protocol *cdp.Error
	return errors.As(err, &protocol) && protocol.Code == -32602 && protocol.Message == "Invalid InterceptionId."
}
