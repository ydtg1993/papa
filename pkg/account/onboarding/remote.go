package onboarding

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

// RemoteOptions is supplied by business code, never by the HTTP caller. The
// caller supplies the same identity/proxy options as the intended runtime and
// must declare login and third-party authorization origins explicitly.
type RemoteOptions struct {
	Pool              *browser.Pool
	URL               string
	AllowedOrigins    []string
	Direct            bool
	PageOptions       browser.PageOptions
	Width             int
	Height            int
	DisconnectTimeout time.Duration
}
type Remote struct {
	mu         sync.Mutex
	opts       RemoteOptions
	allowed    map[string]bool
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	browser    *rod.Browser
	page       *rod.Page
	pages      map[string]*rod.Page
	controller string
	lastSeen   time.Time
	lastFrame  time.Time
	lastInput  time.Time
}
type RemoteRequest struct {
	ID     string  `json:"id"`
	Action string  `json:"action"`
	Lease  string  `json:"lease,omitempty"`
	Page   string  `json:"page,omitempty"`
	X      float64 `json:"x,omitempty"` // normalized CSS viewport coordinates
	Y      float64 `json:"y,omitempty"`
	DeltaY float64 `json:"delta_y,omitempty"`
	Text   string  `json:"text,omitempty"`
	Key    string  `json:"key,omitempty"`
}
type RemotePage struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type RemoteFrame struct {
	Lease             string       `json:"lease"`
	Width             int          `json:"width"`
	Height            int          `json:"height"`
	ImageDataURL      string       `json:"image_data_url,omitempty"`
	Pages             []RemotePage `json:"pages"`
	Page              string       `json:"page"`
	DisconnectSeconds int          `json:"disconnect_seconds"`
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// NewRemote borrows a pool process and isolates all login cookies/storage in a
// temporary browser context. Close/cancellation disposes the context before
// releasing the process. It never exposes a CDP address or a generic evaluator.
func NewRemote(ctx context.Context, opts RemoteOptions) (*Remote, error) {
	if opts.Pool == nil || origin(opts.URL) == "" {
		return nil, errors.New("remote login requires pool and declared URL")
	}
	if scope, ok := identityruntime.FromContext(ctx); ok && scope.Snapshot() != nil {
		opts.PageOptions.Identity = scope.Snapshot()
	}
	if identity := opts.PageOptions.Identity; identity != nil {
		view := identity.Profile.Viewport
		if (opts.Width != 0 && opts.Width != view.Width) || (opts.Height != 0 && opts.Height != view.Height) {
			return nil, errors.New("remote viewport conflicts with pinned browser identity")
		}
		opts.Width, opts.Height = view.Width, view.Height
		if float64(view.Width*view.Height)*identity.Profile.DeviceScaleFactor*identity.Profile.DeviceScaleFactor > 3840*2160 {
			return nil, errors.New("pinned browser identity exceeds remote frame budget")
		}
	}
	if opts.Width == 0 {
		opts.Width = 1280
	}
	if opts.Height == 0 {
		opts.Height = 720
	}
	if opts.DisconnectTimeout == 0 {
		opts.DisconnectTimeout = 30 * time.Second
	}
	if opts.Width < 320 || opts.Width > 1920 || opts.Height < 240 || opts.Height > 1080 || opts.DisconnectTimeout < time.Second || opts.DisconnectTimeout > 2*time.Minute {
		return nil, errors.New("invalid remote login budget")
	}
	run, cancel := context.WithCancel(ctx)
	r := &Remote{opts: opts, allowed: map[string]bool{}, ctx: run, cancel: cancel, done: make(chan struct{}), pages: map[string]*rod.Page{}, lastSeen: time.Now()}
	for _, raw := range opts.AllowedOrigins {
		if origin(raw) != raw {
			cancel()
			return nil, errors.New("remote allowlist must contain exact HTTP origins")
		}
		r.allowed[raw] = true
	}
	if !r.allowed[origin(opts.URL)] {
		cancel()
		return nil, ErrDenied
	}
	ready := make(chan error, 1)
	go func() {
		defer close(r.done)
		defer cancel()
		borrow := opts.Pool.WithIsolatedBrowser
		if opts.Direct {
			borrow = opts.Pool.WithDirectIsolatedBrowser
		}
		err := borrow(run, func(b *browser.Browser) error {
			pageOpts := opts.PageOptions
			if scope, ok := identityruntime.FromContext(run); ok && scope.Snapshot() != nil {
				pageOpts.Identity = scope.Snapshot()
			}
			pageOpts.GovernanceOrigins = append([]string(nil), opts.AllowedOrigins...)
			page, err := b.NewPageWithOptions(run, opts.URL, pageOpts)
			if err != nil {
				return err
			}
			if pageOpts.Identity == nil {
				if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: opts.Width, Height: opts.Height, DeviceScaleFactor: 1}); err != nil {
					return err
				}
			}
			r.mu.Lock()
			r.browser = b.Browser
			r.page = page
			r.mu.Unlock()
			ready <- nil
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-run.Done():
					return nil
				case <-ticker.C:
					r.mu.Lock()
					expired := time.Since(r.lastSeen) > opts.DisconnectTimeout
					r.mu.Unlock()
					if expired {
						cancel()
						return nil
					}
				}
			}
		})
		if err != nil {
			select {
			case ready <- &accountRemoteFailure{}:
			default:
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-r.done
			return nil, err
		}
		return r, nil
	case <-ctx.Done():
		cancel()
		<-r.done
		return nil, ctx.Err()
	}
}

type accountRemoteFailure struct{}

func (*accountRemoteFailure) Error() string { return "remote login browser failed" }
func (r *Remote) Close()                    { r.cancel(); <-r.done }
func (r *Remote) Done() <-chan struct{}     { return r.ctx.Done() }

// WithPage is exclusively for trusted provider code exporting and verifying
// login results. No HTTP request accepts its callback or an arbitrary script.
func (r *Remote) WithPage(ctx context.Context, fn func(*rod.Page) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return ErrGone
	}
	if err := r.checkPage(ctx, r.page); err != nil {
		return err
	}
	return fn(r.page.Context(ctx))
}
func (r *Remote) checkPage(ctx context.Context, page *rod.Page) error {
	if page == nil {
		return ErrGone
	}
	info, err := page.Context(ctx).Info()
	if err != nil {
		return ErrGone
	}
	if !r.allowed[origin(info.URL)] {
		return ErrDenied
	}
	return nil
}
func (r *Remote) request(ctx context.Context, in RemoteRequest) (RemoteFrame, error) {
	// Do not queue unbounded HTTP goroutines behind a slow screenshot/input.
	if !r.mu.TryLock() {
		return RemoteFrame{}, ErrBusy
	}
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return RemoteFrame{}, ErrGone
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if in.Action == "claim" {
		if r.controller != "" && in.Lease != r.controller {
			return RemoteFrame{}, ErrBusy
		}
		if r.controller == "" {
			var err error
			r.controller, err = randomID()
			if err != nil {
				return RemoteFrame{}, err
			}
		}
	} else if r.controller == "" || in.Lease != r.controller {
		return RemoteFrame{}, ErrDenied
	}
	r.lastSeen = time.Now()
	if in.Action == "release" {
		r.cancel()
		return RemoteFrame{}, nil
	}
	if err := r.checkPage(ctx, r.page); err != nil {
		r.cancel()
		return RemoteFrame{}, err
	}
	targets, err := (proto.TargetGetTargets{}).Call(r.browser.Context(ctx))
	if err != nil {
		return RemoteFrame{}, ErrGone
	}
	// Rod.Pages enumerates every context in the process. Filter before
	// attaching, otherwise one login could see another context's same-origin
	// tabs. Keep page lifetimes on the process context, not an HTTP request.
	pages := []*rod.Page{}
	for _, target := range targets.TargetInfos {
		if target.Type != proto.TargetTargetInfoTypePage || target.BrowserContextID != r.browser.BrowserContextID {
			continue
		}
		p, attachErr := r.browser.PageFromTarget(target.TargetID)
		if attachErr != nil {
			return RemoteFrame{}, ErrGone
		}
		pages = append(pages, p)
	}
	if len(pages) > 8 {
		r.cancel()
		return RemoteFrame{}, errors.New("remote login page limit exceeded")
	}
	next := map[string]*rod.Page{}
	for _, p := range pages {
		if r.checkPage(ctx, p) != nil {
			continue
		}
		// Chromium-created popups have not passed the managed identity's
		// before-navigation initialization. Never present them as identity-safe.
		if r.opts.PageOptions.Identity != nil && browser.ManagedIdentity(p) == nil {
			continue
		}
		id := ""
		for k, known := range r.pages {
			if known.TargetID == p.TargetID {
				id = k
				break
			}
		}
		if id == "" {
			id, err = randomID()
			if err != nil {
				return RemoteFrame{}, err
			}
		}
		next[id] = p
	}
	r.pages = next
	if in.Action == "select" {
		page := r.pages[in.Page]
		if page == nil {
			return RemoteFrame{}, ErrDenied
		}
		r.page = page
		if r.opts.PageOptions.Identity == nil {
			if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: r.opts.Width, Height: r.opts.Height, DeviceScaleFactor: 1}); err != nil {
				return RemoteFrame{}, ErrGone
			}
		}
	}
	page := r.page.Context(ctx)
	switch in.Action {
	case "claim", "frame", "select":
	case "click", "text", "key", "scroll":
		if time.Since(r.lastInput) < 15*time.Millisecond {
			return RemoteFrame{}, ErrBusy
		}
		r.lastInput = time.Now()
		switch in.Action {
		case "click", "scroll":
			if math.IsNaN(in.X) || math.IsNaN(in.Y) || in.X < 0 || in.X >= 1 || in.Y < 0 || in.Y >= 1 {
				return RemoteFrame{}, errors.New("remote coordinates outside viewport")
			}
			event := proto.InputDispatchMouseEvent{X: in.X * float64(r.opts.Width), Y: in.Y * float64(r.opts.Height)}
			if in.Action == "scroll" {
				if math.IsNaN(in.DeltaY) || math.Abs(in.DeltaY) > 720 {
					return RemoteFrame{}, errors.New("remote scroll outside budget")
				}
				event.Type = proto.InputDispatchMouseEventTypeMouseWheel
				event.DeltaY = in.DeltaY
				err = event.Call(page)
			} else {
				event.Type = proto.InputDispatchMouseEventTypeMousePressed
				event.Button = proto.InputMouseButtonLeft
				event.ClickCount = 1
				err = event.Call(page)
				if err == nil {
					event.Type = proto.InputDispatchMouseEventTypeMouseReleased
					err = event.Call(page)
				}
			}
		case "text":
			if len(in.Text) > 4096 {
				return RemoteFrame{}, errors.New("remote text too large")
			}
			err = (proto.InputInsertText{Text: in.Text}).Call(page)
		case "key":
			codes := map[string]int{"Enter": 13, "Tab": 9, "Backspace": 8, "Escape": 27, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "Delete": 46}
			code, ok := codes[in.Key]
			if !ok {
				return RemoteFrame{}, errors.New("remote key is not allowed")
			}
			event := proto.InputDispatchKeyEvent{Type: proto.InputDispatchKeyEventTypeKeyDown, Key: in.Key, WindowsVirtualKeyCode: code}
			err = event.Call(page)
			if err == nil {
				event.Type = proto.InputDispatchKeyEventTypeKeyUp
				err = event.Call(page)
			}
		}
		if err != nil {
			return RemoteFrame{}, ErrGone
		}
	default:
		return RemoteFrame{}, errors.New("remote action is not allowed")
	}
	out := RemoteFrame{Lease: r.controller, Width: r.opts.Width, Height: r.opts.Height, Pages: []RemotePage{}, DisconnectSeconds: int(r.opts.DisconnectTimeout.Seconds())}
	ids := make([]string, 0, len(r.pages))
	for id := range r.pages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		label := "登录页面 " + string(rune('1'+i))
		out.Pages = append(out.Pages, RemotePage{ID: id, Label: label})
		if r.pages[id].TargetID == r.page.TargetID {
			out.Page = id
		}
	}
	if in.Action == "claim" || in.Action == "frame" || in.Action == "select" {
		if !r.lastFrame.IsZero() && time.Since(r.lastFrame) < 300*time.Millisecond {
			return out, ErrBusy
		}
		r.lastFrame = time.Now()
		shot, err := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
		if err != nil {
			return RemoteFrame{}, ErrGone
		}
		if len(shot) > 4<<20 {
			return RemoteFrame{}, errors.New("remote frame exceeds budget")
		}
		out.ImageDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(shot)
	}
	return out, nil
}
func (s *Service) Remote(ctx context.Context, owner string, in RemoteRequest) (RemoteFrame, error) {
	p, err := s.lookup(owner, in.ID)
	if err != nil {
		return RemoteFrame{}, err
	}
	p.mu.Lock()
	r := p.remote
	active := p.ended.IsZero() && p.ctx.Err() == nil && p.view.State == "remote"
	p.mu.Unlock()
	if !active || r == nil {
		return RemoteFrame{}, ErrGone
	}
	return r.request(ctx, in)
}
