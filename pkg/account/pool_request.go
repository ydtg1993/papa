package account

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

func (m *Manager) poolAttempt(ctx context.Context, key PoolKey, t Transport) (*Pools, *Attempt, error) {
	p := m.Pools()
	if p == nil {
		return nil, nil, ErrPoolUnknown
	}
	if a, ok := AttemptFromContext(ctx); ok {
		if a.Pool != key || a.owner != p || (a.Transport != Both && a.Transport != t) {
			return nil, nil, ErrAttemptChanged
		}
		if err := p.ValidateAttempt(a); err != nil {
			return nil, nil, err
		}
		return p, a, nil
	}
	a, err := p.Acquire(ctx, key, t)
	return p, a, err
}

func (p *Pools) requestContext(ctx context.Context, a *Attempt) context.Context {
	if p.options.Context != nil {
		ctx = p.options.Context(ctx, a.Account)
	}
	return WithAttempt(ctx, a)
}

// DoPool performs exactly one exchange with the task attempt's identity. Explicit
// expiry ends this call and asynchronously schedules failover; GET is not replayed.
// Subsequent calls must use WithAttempt to retain identity across a task.
func (m *Manager) DoPool(ctx context.Context, key PoolKey, req htmlfetch.Request) (*htmlfetch.Response, error) {
	p, a, err := m.poolAttempt(ctx, key, HTTP)
	if err != nil {
		return nil, err
	}
	ctx = p.requestContext(ctx, a)
	ctx, releaseIdentity, identityErr := m.PinIdentity(ctx, a.Account)
	if identityErr != nil {
		return nil, identityErr
	}
	defer releaseIdentity()
	e, err := m.get(a.Account)
	if err != nil {
		return nil, err
	}
	if err = e.validateIdentityState(ctx); err != nil {
		return nil, err
	}
	o, err := origin(req.URL)
	if err != nil || !e.origins[o] {
		return nil, errors.New("request outside account origins")
	}
	ctx, err = e.context(ctx)
	if err != nil {
		return nil, err
	}
	if err = e.enter(ctx); err != nil {
		return nil, err
	}
	defer e.leave()
	if err = p.ValidateAttempt(a); err != nil {
		return nil, err
	}
	e.mu.Lock()
	c := cloneCredentials(e.current)
	version := e.version
	e.mu.Unlock()
	if c == nil || version != a.CredentialVersion {
		return nil, ErrAttemptChanged
	}
	r := req
	r.NoRedirect = true
	r.Headers = map[string]string{"Cookie": "", "Proxy-Authorization": ""}
	for h := range e.authHeaders {
		r.Headers[h] = ""
	}
	for k, v := range req.Headers {
		k = http.CanonicalHeaderKey(k)
		if e.authHeaders[k] || k == "Cookie" || k == "Proxy-Authorization" {
			return nil, errors.New("request cannot override account credentials")
		}
		r.Headers[k] = v
	}
	if e.config.UserAgent != "" {
		r.Headers["User-Agent"] = e.config.UserAgent
	}
	for k, v := range c.Headers {
		r.Headers[k] = v
	}
	response, fetchErr := m.client.Do(htmlfetch.WithSession(ctx, c.Session), r)
	if errors.Is(fetchErr, core.ErrSiteChallenge) {
		return response, fetchErr
	}
	if e.config.ProxyURL != "" {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		failure := proxy.ClassifyHTTP(e.config.ProxyURL, status, fetchErr)
		if failure == proxy.TransportFailure || failure == proxy.AuthenticationFailure || errors.Is(fetchErr, proxy.ErrUnavailable) {
			p.ReportFailure(a, ProxyUnavailable, a.TaskID)
			return response, &Failure{Code: "fixed-proxy-unavailable", Cause: errors.Join(ErrFixedProxyUnavailable, fetchErr)}
		}
	}
	outcome := e.config.Classify(response, fetchErr)
	switch outcome {
	case Expired:
		p.ReportFailure(a, Expired, a.TaskID)
		return response, ErrExpired
	case Challenge:
		p.ReportFailure(a, Challenge, a.TaskID)
		return response, ErrChallenge
	case Forbidden:
		return response, ErrForbidden
	case Valid, Unknown:
	default:
		return response, errors.New("unknown account response classification")
	}
	if err = e.observe(ctx, version, response, outcome == Valid && fetchErr == nil); err != nil {
		return response, err
	}
	return response, fetchErr
}

func (m *Manager) FetchPoolHTML(ctx context.Context, key PoolKey, rawURL string) (*htmlfetch.Page, error) {
	r, err := m.DoPool(ctx, key, htmlfetch.Request{URL: rawURL})
	if err != nil {
		return nil, err
	}
	page, err := htmlfetch.NewRenderedPage(r.URL.String(), string(r.Body))
	if err != nil {
		return nil, fmt.Errorf("parse account HTML: %w", err)
	}
	page.StatusCode = r.StatusCode
	page.ContentType = r.Header.Get("Content-Type")
	return page, nil
}

// WithPoolBrowser restores and checks the bound account once. Only the independent
// coordinator may log in or replace an expired identity. The callback is never
// replayed, and browser-only accounts do not require HTTPReady.
func (m *Manager) WithPoolBrowser(ctx context.Context, key PoolKey, fn func(*rod.Page) error) error {
	return m.withPoolBrowser(ctx, key, nil, fn)
}

func (m *Manager) withPoolBrowser(ctx context.Context, key PoolKey, options *browser.PageOptions, fn func(*rod.Page) error) error {
	if fn == nil {
		return errors.New("account browser callback required")
	}
	if core.ProxyURLFrom(ctx) != "" {
		return errors.New("account browser requires a pool-bound proxy, not a request proxy")
	}
	p, a, err := m.poolAttempt(ctx, key, Browser)
	if err != nil {
		return err
	}
	ctx = p.requestContext(ctx, a)
	ctx, releaseIdentity, identityErr := m.PinIdentity(ctx, a.Account)
	if identityErr != nil {
		return identityErr
	}
	defer releaseIdentity()
	e, err := m.get(a.Account)
	if err != nil {
		return err
	}
	if e.config.Browser == nil {
		return ErrUnsupported
	}
	if err = e.validateIdentityState(ctx); err != nil {
		return err
	}
	if err = e.enter(ctx); err != nil {
		return err
	}
	defer e.leave()
	if err = e.enterBrowser(ctx); err != nil {
		return err
	}
	defer func() { <-e.browserSlot }()
	if err = p.ValidateAttempt(a); err != nil {
		return err
	}
	e.mu.Lock()
	state, err := clonePageState(e.pageState)
	revision, browserRevision := e.revision, e.browserRevision
	e.mu.Unlock()
	if err != nil {
		return &Failure{Code: "invalid-browser-state", Cause: err}
	}
	if state == nil {
		return ErrAttemptChanged
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	var saved *browser.PageState
	config := *e.config.Browser
	if options != nil {
		if options.Block != nil {
			config.Options.Block = options.Block
		}
		if len(options.GovernanceOrigins) > 0 {
			config.Options.GovernanceOrigins = append(append([]string(nil), config.Options.GovernanceOrigins...), options.GovernanceOrigins...)
		}
	}
	err = useBrowser(ctx, &config, state, func(page *rod.Page) error {
		valid, checkErr := e.config.Browser.Check(ctx, page)
		if checkErr != nil {
			return &Failure{Code: "browser-check-failed", Cause: checkErr}
		}
		if err := browserOrigin(page, e.config.Browser.URL); err != nil {
			return err
		}
		if !valid {
			return ErrExpired
		}
		if err := p.ValidateAttempt(a); err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
		if err := browserOrigin(page, e.config.Browser.URL); err != nil {
			return err
		}
		var err error
		saved, err = exportBrowserState(page, e.config.Browser)
		return err
	})
	if identityOrSiteFailure(err) {
		return err
	}
	if errors.Is(err, ErrExpired) {
		p.ReportFailure(a, Expired, a.TaskID)
	} else if errors.Is(err, ErrChallenge) {
		p.ReportFailure(a, Challenge, a.TaskID)
	} else if errors.Is(err, ErrFixedProxyUnavailable) {
		p.ReportFailure(a, ProxyUnavailable, a.TaskID)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision || e.browserRevision != browserRevision || e.paused {
		return nil
	} // completed old request must not resurrect invalidated state
	e.pageState = saved
	e.browserCheck = time.Now()
	e.browserRevision++
	return e.saveLocked(ctx)
}

// NavigatePoolPage uses the verified account page and its dedicated browser pool.
// The destination must share the bootstrap origin; Cookie/authentication headers
// remain owned by the account configuration and cannot be overridden by a task.
func (m *Manager) NavigatePoolPage(ctx context.Context, key PoolKey, rawURL string, opts browser.PageOptions, fn func(*rod.Page) error) error {
	_, a, err := m.poolAttempt(ctx, key, Browser)
	if err != nil {
		return err
	}
	opts, err = m.poolPageOptions(a, rawURL, opts)
	if err != nil {
		return err
	}
	ctx = WithAttempt(ctx, a)
	return m.withPoolBrowser(ctx, key, &opts, func(page *rod.Page) error {
		navigation := opts
		navigation.Block = nil
		navigation.GovernanceOrigins = nil
		if err := browser.NavigatePage(ctx, page, rawURL, navigation); err != nil {
			return err
		}
		return fn(page)
	})
}

func (m *Manager) poolPageOptions(a *Attempt, rawURL string, opts browser.PageOptions) (browser.PageOptions, error) {
	e, err := m.get(a.Account)
	if err != nil {
		return opts, err
	}
	if e.config.Browser == nil {
		return opts, ErrUnsupported
	}
	destination, err := origin(rawURL)
	bootstrap, _ := origin(e.config.Browser.URL)
	if err != nil || destination != bootstrap {
		return opts, errors.New("account render outside browser bootstrap origin")
	}
	if len(opts.Cookies) > 0 {
		return opts, errors.New("account render cannot override cookies")
	}
	protected := func(header string) bool {
		header = http.CanonicalHeaderKey(header)
		return e.authHeaders[header] || header == "Cookie" || header == "Proxy-Authorization" || header == "User-Agent"
	}
	headers := maps.Clone(e.config.Browser.Options.Headers)
	if headers == nil {
		headers = map[string]string{}
	}
	for h, v := range opts.Headers {
		if protected(h) {
			return opts, errors.New("account render cannot override identity headers")
		}
		headers[http.CanonicalHeaderKey(h)] = v
	}
	for _, h := range opts.RemoveHeaders {
		if protected(h) {
			return opts, errors.New("account render cannot remove identity headers")
		}
		delete(headers, http.CanonicalHeaderKey(h))
	}
	opts.Headers = headers
	// Header removal is resolved against the account's detached configured set;
	// ConfigurePage receives the complete replacement, including an empty map.
	opts.RemoveHeaders = nil
	return opts, nil
}
