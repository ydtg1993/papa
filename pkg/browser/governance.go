package browser

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/governance"
)

// Physical capacity has already been reserved when this is called. Thus a full
// account-specific pool cannot hold global browser capacity while it queues.
func (p *Pool) acquireBrowserPermit(ctx context.Context) (func(), error) {
	c, site := governance.FromContext(ctx), governance.SiteFromContext(ctx)
	if c == nil || site == "" {
		return func() {}, nil
	}
	p.mu.Lock()
	if p.admissionCtx == nil {
		p.admissionCtx, p.admissionCancel = context.WithCancel(context.Background())
	}
	poolCtx := p.admissionCtx
	if p.closed.Load() {
		p.admissionCancel()
	}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(poolCtx, cancel)
	defer stop()
	return c.Acquire(ctx, site, governance.ClassBrowser)
}
func (b *Browser) setBorrowRelease(release func()) {
	b.mu.Lock()
	b.borrowRelease = release
	b.mu.Unlock()
}
func (b *Browser) releaseBorrow() {
	b.mu.Lock()
	release := b.borrowRelease
	b.borrowRelease = nil
	b.mu.Unlock()
	if release != nil {
		release()
	}
}

type browserExchange struct {
	release  func(governance.Feedback)
	started  time.Time
	observed bool
}
type pageGovernance struct {
	mu             sync.Mutex
	modeMu         sync.Mutex
	controlledMode bool
	page           *rod.Page
	controller     *governance.Controller
	site           string
	origins        map[string]bool
	defaultOrigin  string
	blocked        map[proto.NetworkResourceType]bool
	active         map[proto.NetworkRequestID]*browserExchange
	closed         bool
}

func requestOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return u.Scheme + "://" + host
}

func applyPageGovernance(ctx context.Context, page *rod.Page, opts PageOptions) error {
	c, site := governance.FromContext(ctx), governance.SiteFromContext(ctx)
	if c == nil || site == "" {
		return applyBlock(page, opts.Block)
	}
	g := &pageGovernance{page: page, controller: c, site: site, origins: make(map[string]bool), blocked: make(map[proto.NetworkResourceType]bool), active: make(map[proto.NetworkRequestID]*browserExchange)}
	for _, raw := range opts.GovernanceOrigins {
		origin := requestOrigin(raw)
		if origin == "" {
			return fmt.Errorf("invalid browser governance origin")
		}
		g.origins[origin] = true
	}
	if opts.Block != nil {
		for _, typ := range opts.Block.ResourceTypes {
			g.blocked[typ] = true
		}
		if err := applyBlock(page, &BlockOptions{URLPatterns: opts.Block.URLPatterns}); err != nil {
			return err
		}
	}
	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		return fmt.Errorf("enable request observations: %w", err)
	}
	if err := g.applyNetworkMode(); err != nil {
		return err
	}
	events := page.Event() // subscribe before enabling request interception
	if err := (proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{{URLPattern: "*", RequestStage: proto.FetchRequestStageRequest}}}).Call(page); err != nil {
		return fmt.Errorf("enable request admission: %w", err)
	}
	go g.run(events)
	go g.watchPolicy()
	return nil
}

func (g *pageGovernance) applyNetworkMode() error {
	g.modeMu.Lock()
	defer g.modeMu.Unlock()
	enabled := g.controller.Effective(g.site).Request.Enabled
	if enabled == g.controlledMode {
		return nil
	}
	if err := (proto.NetworkSetBypassServiceWorker{Bypass: enabled}).Call(g.page); err != nil {
		return fmt.Errorf("update governed service worker mode: %w", err)
	}
	if err := (proto.NetworkSetCacheDisabled{CacheDisabled: enabled}).Call(g.page); err != nil {
		return fmt.Errorf("update governed cache mode: %w", err)
	}
	g.controlledMode = enabled
	return nil
}
func (g *pageGovernance) watchPolicy() {
	for {
		changed := g.controller.PolicyChanges()
		if err := g.applyNetworkMode(); err != nil {
			return
		}
		select {
		case <-g.page.GetContext().Done():
			return
		case <-changed:
		}
	}
}

func (g *pageGovernance) run(events <-chan *rod.Message) {
	defer g.close()
	for message := range events {
		switch message.Method {
		case "Fetch.requestPaused":
			e := &proto.FetchRequestPaused{}
			message.Load(e)
			// Never block the event reader on admission: completion events release
			// capacity needed by these paused requests.
			go g.admit(e)
		case "Network.responseReceived":
			e := &proto.NetworkResponseReceived{}
			message.Load(e)
			g.observe(e.RequestID, e.Response)
		case "Network.requestWillBeSent":
			e := &proto.NetworkRequestWillBeSent{}
			message.Load(e)
			if e.RedirectResponse != nil {
				g.observe(e.RequestID, e.RedirectResponse)
				g.finish(e.RequestID, false)
			}
		case "Network.loadingFinished":
			e := &proto.NetworkLoadingFinished{}
			message.Load(e)
			g.finish(e.RequestID, false)
		case "Network.loadingFailed":
			e := &proto.NetworkLoadingFailed{}
			message.Load(e)
			g.finish(e.RequestID, true)
		}
	}
}

func (g *pageGovernance) admit(e *proto.FetchRequestPaused) {
	if e.Request == nil {
		return
	}
	fail := func() {
		_ = (proto.FetchFailRequest{RequestID: e.RequestID, ErrorReason: proto.NetworkErrorReasonBlockedByClient}).Call(g.page)
	}
	if g.blocked[e.ResourceType] {
		fail()
		return
	}
	if err := g.applyNetworkMode(); err != nil {
		fail()
		return
	}
	origin := requestOrigin(e.Request.URL)
	g.mu.Lock()
	if g.defaultOrigin == "" && e.ResourceType == proto.NetworkResourceTypeDocument && origin != "" {
		g.defaultOrigin = origin
	}
	controlled := origin != "" && (origin == g.defaultOrigin || g.origins[origin])
	closed := g.closed
	g.mu.Unlock()
	if closed {
		fail()
		return
	}
	if !controlled {
		_ = (proto.FetchContinueRequest{RequestID: e.RequestID}).Call(g.page)
		return
	}
	// A corresponding Network id is needed to retain admission until the
	// response body finishes, including cancellation and redirects.
	if e.NetworkID == "" {
		fail()
		return
	}
	release, err := g.controller.AcquireRequest(g.page.GetContext(), g.site)
	if err != nil {
		fail()
		return
	}
	active := &browserExchange{release: release, started: time.Now()}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		release(governance.Feedback{})
		fail()
		return
	}
	g.active[e.NetworkID] = active
	g.mu.Unlock()
	if err := (proto.FetchContinueRequest{RequestID: e.RequestID}).Call(g.page); err != nil {
		g.finish(e.NetworkID, true)
	}
}

func (g *pageGovernance) observe(id proto.NetworkRequestID, response *proto.NetworkResponse) {
	if response == nil {
		return
	}
	g.mu.Lock()
	active := g.active[id]
	if active == nil || active.observed {
		g.mu.Unlock()
		return
	}
	active.observed = true
	latency := time.Since(active.started)
	g.mu.Unlock()
	retryAfter := ""
	for key, value := range response.Headers {
		if strings.EqualFold(key, "Retry-After") {
			retryAfter = value.Str()
			break
		}
	}
	g.controller.Observe(g.site, governance.Feedback{StatusCode: response.Status, Latency: latency, RetryAfter: retryAfter})
}
func (g *pageGovernance) finish(id proto.NetworkRequestID, failed bool) {
	g.mu.Lock()
	active := g.active[id]
	delete(g.active, id)
	g.mu.Unlock()
	if active == nil {
		return
	}
	f := governance.Feedback{}
	if failed && !active.observed {
		f.Failed = true
		f.Latency = time.Since(active.started)
	}
	active.release(f)
}
func (g *pageGovernance) close() {
	g.mu.Lock()
	g.closed = true
	active := g.active
	g.active = make(map[proto.NetworkRequestID]*browserExchange)
	g.mu.Unlock()
	for _, request := range active {
		request.release(governance.Feedback{})
	}
}
