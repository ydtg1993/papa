package account

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/discovery"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"golang.org/x/net/idna"
)

func origin(raw string) (string, error) {
	s, err := discovery.CanonicalURL(raw, "")
	if err != nil {
		return "", errors.New("invalid account URL")
	}
	u, _ := url.Parse(s)
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", errors.New("invalid account hostname")
		}
		if port := u.Port(); port != "" {
			u.Host = net.JoinHostPort(host, port)
		} else {
			u.Host = host
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

func (m *Manager) Register(key Key, cfg Config) error {
	if strings.TrimSpace(key.Site) == "" || strings.TrimSpace(key.ID) == "" || len(key.Site) > 256 || len(key.ID) > 256 {
		return errors.New("account site and ID required (max 256 bytes each)")
	}
	if len(cfg.Origins) == 0 {
		return errors.New("account origins required")
	}
	autoBridge := cfg.Login == nil && cfg.Check != nil && cfg.Browser != nil
	if autoBridge {
		cfg.Login = browserLogin(*cfg.Browser)
	}
	if (cfg.Login == nil) != (cfg.Check == nil) {
		return errors.New("HTTP Login and Check must be configured together")
	}
	if cfg.Login == nil && cfg.Browser == nil {
		return ErrUnsupported
	}
	if cfg.Login != nil && m.client == nil {
		return errors.New("account HTTP client required")
	}
	if cfg.Timeout < 0 || cfg.FailureCooldown < 0 || cfg.CheckInterval < 0 || cfg.MaxConcurrent < 0 {
		return errors.New("negative account budget")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.FailureCooldown == 0 {
		cfg.FailureCooldown = 5 * time.Second
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.MaxConcurrent > 1024 {
		return errors.New("account concurrency exceeds 1024")
	}
	if cfg.Classify == nil {
		cfg.Classify = defaultClassify
	}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return errors.New("invalid account proxy")
		}
	}
	e := &entry{config: cfg, key: key, origins: map[string]bool{}, authHeaders: map[string]bool{"Authorization": true, "X-Csrf-Token": true}, slots: make(chan struct{}, cfg.MaxConcurrent), browserSlot: make(chan struct{}, 1)}
	e.autoBridge = autoBridge
	for _, raw := range cfg.Origins {
		o, err := origin(raw)
		if err != nil {
			return err
		}
		u, parseErr := url.Parse(strings.TrimSpace(raw))
		if parseErr != nil {
			return errors.New("invalid account origin")
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("account scope must be an origin")
		}
		e.origins[o] = true
	}
	for _, h := range cfg.AuthHeaders {
		h = http.CanonicalHeaderKey(h)
		if !validHeader(h) || h == "Cookie" || h == "Host" || h == "Proxy-Authorization" || h == "User-Agent" || h == "Content-Length" {
			return errors.New("invalid account credential header")
		}
		e.authHeaders[h] = true
	}
	if strings.ContainsAny(cfg.UserAgent, "\r\n") {
		return errors.New("invalid account user agent")
	}
	if cfg.Browser != nil {
		b := *cfg.Browser
		if b.Pool == nil || b.Login == nil || b.Check == nil {
			return errors.New("browser pool, Login and Check required")
		}
		o, err := origin(b.URL)
		if err != nil || !e.origins[o] {
			return errors.New("browser bootstrap outside account origins")
		}
		if b.Options.Interact != nil {
			return errors.New("use account browser callback instead of PageOptions.Interact")
		}
		if len(b.Options.Cookies) > 0 {
			return errors.New("use browser login/state instead of shared PageOptions.Cookies")
		}
		b.Options.Headers = maps.Clone(b.Options.Headers)
		b.CookieURLs = append([]string(nil), b.CookieURLs...)
		if cfg.UserAgent != "" {
			if b.Options.Headers == nil {
				b.Options.Headers = map[string]string{}
			}
			b.Options.Headers["User-Agent"] = cfg.UserAgent
		}
		for _, raw := range b.CookieURLs {
			o, err := origin(raw)
			if err != nil || !e.origins[o] {
				return errors.New("cookie bridge outside account origins")
			}
		}
		e.config.Browser = &b
		if autoBridge {
			e.config.Login = browserLogin(b)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; ok {
		return errors.New("account already registered")
	}
	m.entries[key] = e
	return nil
}

func validHeader(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}
func (m *Manager) get(key Key) (*entry, error) {
	m.mu.RLock()
	e := m.entries[key]
	m.mu.RUnlock()
	if e == nil {
		return nil, ErrUnknown
	}
	return e, nil
}
func (e *entry) enter(ctx context.Context) error {
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	paused := e.paused
	e.mu.Unlock()
	if paused {
		<-e.slots
		return ErrPaused
	}
	if err := ctx.Err(); err != nil {
		<-e.slots
		return err
	}
	return nil
}
func (e *entry) leave() { <-e.slots }

func (e *entry) context(ctx context.Context) (context.Context, error) {
	if p := core.ProxyURLFrom(ctx); p != "" && p != e.config.ProxyURL {
		return nil, errors.New("request proxy conflicts with account")
	}
	if e.config.ProxyURL == "" {
		ctx = htmlfetch.WithProxy(ctx, false)
	} else {
		ctx = core.WithProxyURL(ctx, e.config.ProxyURL)
	}
	headers := map[string]string{"Cookie": "", "Proxy-Authorization": ""}
	for h := range e.authHeaders {
		headers[h] = ""
	}
	if e.config.UserAgent != "" {
		headers["User-Agent"] = e.config.UserAgent
	}
	return core.WithHeaders(ctx, headers), nil
}

func (e *entry) checkCredentials(c *Credentials) error {
	if c == nil || c.Session == nil {
		return errors.New("candidate session missing")
	}
	normal := map[string]string{}
	for k, v := range c.Headers {
		k = http.CanonicalHeaderKey(k)
		if !e.authHeaders[k] || strings.ContainsAny(v, "\r\n") {
			return errors.New("unregistered credential header")
		}
		if _, ok := normal[k]; ok {
			return errors.New("duplicate credential header")
		}
		normal[k] = v
	}
	c.Headers = normal
	c.Values = maps.Clone(c.Values)
	if c.BrowserState != nil {
		state, err := clonePageState(c.BrowserState)
		if err != nil || !e.origins[state.Origin] {
			return errors.New("invalid candidate browser state")
		}
		c.BrowserState = state
	}
	return nil
}
func cloneCredentials(c *Credentials) *Credentials {
	if c == nil {
		return nil
	}
	return &Credentials{Session: c.Session, Headers: maps.Clone(c.Headers), Values: maps.Clone(c.Values)}
}

func (e *entry) ensure(ctx context.Context) (credentials *Credentials, version uint64, err error) {
	// Automatic Rod authentication owns the browser slot through HTTP verification
	// and publication. Register this cleanup before finalization so it runs last.
	browserLocked := false
	defer func() {
		if browserLocked {
			<-e.browserSlot
		}
	}()
	e.mu.Lock()
	if e.paused {
		e.mu.Unlock()
		return nil, 0, ErrPaused
	}
	if e.failure != nil && time.Now().Before(e.retryAt) {
		err = e.failure
		e.mu.Unlock()
		return nil, 0, err
	}
	if f := e.flight; f != nil {
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-f.done:
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			return f.credentials, f.version, f.err
		}
	}
	if e.current != nil && e.config.CheckInterval > 0 && time.Since(e.lastCheck) < e.config.CheckInterval {
		c, v := e.current, e.version
		e.mu.Unlock()
		return c, v, nil
	}
	f := &flight{done: make(chan struct{})}
	e.flight = f
	revision := e.revision
	browserRevision := e.browserRevision
	oldVersion := e.version
	old := cloneCredentials(e.current)
	seed := maps.Clone(e.seed)
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	loggedIn := false
	// The default error also releases waiters when a callback invokes Goexit.
	err = &Failure{Code: "authentication-aborted"}
	defer func() {
		p := recover()
		if p != nil {
			err = &Failure{Code: "authentication-panic"}
		}
		if err == nil && credentials == nil {
			err = &Failure{Code: "authentication-aborted"}
		}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		e.mu.Lock()
		finished := false
		defer func() {
			if !finished {
				e.current = nil
				e.version++
				credentials = nil
				err = &Failure{Code: "authentication-finalize-aborted"}
			}
			f.credentials, f.version, f.err = credentials, version, err
			e.flight = nil
			close(f.done)
			e.mu.Unlock()
		}()
		if e.revision != revision || e.version != oldVersion || e.paused ||
			(err == nil && loggedIn && credentials.BrowserState != nil && e.browserRevision != browserRevision) {
			err = ErrSuperseded
		}
		if err == nil {
			e.current = credentials
			e.seed = maps.Clone(credentials.Values)
			e.lastCheck = time.Now()
			if loggedIn {
				e.version++
				e.lastLogin = e.lastCheck
				if credentials.BrowserState != nil {
					e.pageState = credentials.BrowserState
					e.browserCheck = e.lastCheck
					e.browserRevision++
				}
			}
			e.failure = nil
			e.retryAt = time.Time{}
			version = e.version
			if saveErr := e.saveLocked(ctx); saveErr != nil {
				err = saveErr
				e.current = nil
				e.lastCheck = time.Time{}
				e.version++
			}
		}
		if err != nil {
			credentials = nil
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrSuperseded) {
				e.failure = err
				e.retryAt = time.Now().Add(e.config.FailureCooldown)
			}
			if errors.Is(err, ErrChallenge) {
				e.paused = true
				e.revision++
			}
		}
		finished = true
		if p != nil {
			panic(p)
		}
	}()
	if old != nil {
		valid, checkErr := e.config.Check(ctx, old)
		if checkErr != nil {
			return nil, 0, &Failure{Code: "check-failed", Cause: checkErr}
		}
		if valid {
			if err = e.checkCredentials(old); err != nil {
				return nil, 0, &Failure{Code: "invalid-credentials", Cause: err}
			}
			return old, oldVersion, nil
		}
		seed = maps.Clone(old.Values)
	}
	if err = ctx.Err(); err != nil {
		return nil, 0, err
	}
	candidate := &Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{}, Values: seed}
	if candidate.Values == nil {
		candidate.Values = map[string]string{}
	}
	if e.autoBridge {
		if err = e.enterBrowser(ctx); err != nil {
			return nil, 0, err
		}
		browserLocked = true
		e.mu.Lock()
		if e.revision != revision || e.version != oldVersion || e.paused {
			e.mu.Unlock()
			return nil, 0, ErrSuperseded
		}
		if e.failure != nil && time.Now().Before(e.retryAt) {
			err = e.failure
			e.mu.Unlock()
			return nil, 0, err
		}
		candidate.BrowserState, err = clonePageState(e.pageState)
		browserRevision = e.browserRevision
		e.mu.Unlock()
		if err != nil {
			return nil, 0, &Failure{Code: "invalid-browser-state", Cause: err}
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		restored := false
		if e.autoBridge {
			restored, err = browserLoginState(ctx, candidate, *e.config.Browser)
		} else {
			err = e.config.Login(ctx, candidate)
		}
		if err != nil {
			return nil, 0, &Failure{Code: "login-failed", Cause: err}
		}
		if err = e.checkCredentials(candidate); err != nil {
			return nil, 0, &Failure{Code: "invalid-credentials", Cause: err}
		}
		valid, checkErr := e.config.Check(ctx, candidate)
		if checkErr != nil {
			return nil, 0, &Failure{Code: "verification-failed", Cause: checkErr}
		}
		if !valid {
			if restored && attempt == 0 {
				// A browser-valid snapshot may lack an HTTP token or a cookie on
				// another configured origin. Retry once with a clean login candidate.
				candidate = &Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{}, Values: maps.Clone(seed)}
				if candidate.Values == nil {
					candidate.Values = map[string]string{}
				}
				continue
			}
			return nil, 0, ErrExpired
		}
		if err = e.checkCredentials(candidate); err != nil {
			return nil, 0, &Failure{Code: "invalid-credentials", Cause: err}
		}
		loggedIn = true
		return candidate, 0, nil
	}
	return nil, 0, ErrExpired
}

// Invalidate affects only the HTTP credential generation actually used. A late
// response cannot invalidate a newly authenticated account. Returns whether applied.
func (m *Manager) Invalidate(key Key, version uint64) bool {
	e, err := m.get(key)
	if err != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.version != version || e.current == nil {
		return false
	}
	e.seed = maps.Clone(e.current.Values)
	e.current = nil
	e.lastCheck = time.Time{}
	e.version++
	e.failure = nil
	e.retryAt = time.Time{}
	return true
}
func (m *Manager) Pause(key Key) error {
	e, err := m.get(key)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused = true
	e.revision++
	return nil
}
func (m *Manager) Resume(key Key) error {
	e, err := m.get(key)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused = false
	e.failure = nil
	e.retryAt = time.Time{}
	e.lastCheck = time.Time{}
	return nil
}
func (m *Manager) Status() []Status {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.RUnlock()
	out := make([]Status, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		s := Status{Key: e.key, Version: e.version, Paused: e.paused, Authenticating: e.flight != nil, HTTPReady: e.current != nil && !e.lastCheck.IsZero() && !e.paused && e.failure == nil, BrowserReady: e.pageState != nil && !e.browserCheck.IsZero() && !e.paused && e.failure == nil, LastCheck: e.lastCheck, LastLogin: e.lastLogin, LastBrowserCheck: e.browserCheck, RetryAt: e.retryAt}
		s.HTTPConfigured, s.BrowserConfigured = e.config.Login != nil, e.config.Browser != nil
		s.InUse, s.MaxConcurrent = len(e.slots), cap(e.slots)
		if e.failure != nil {
			s.Failure = "authentication-failed"
			var f *Failure
			if errors.As(e.failure, &f) {
				s.Failure = f.Code
			}
			if errors.Is(e.failure, ErrChallenge) {
				s.Failure = "manual-verification"
			}
		}
		e.mu.Unlock()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Site != out[j].Key.Site {
			return out[i].Key.Site < out[j].Key.Site
		}
		return out[i].Key.ID < out[j].Key.ID
	})
	return out
}

// Do runs a bounded authenticated request. Only GET/HEAD are replayed, once, on
// an explicitly classified expiry. Redirects are returned without following.
func (m *Manager) Do(ctx context.Context, key Key, req htmlfetch.Request) (*htmlfetch.Response, error) {
	if a, ok := AttemptFromContext(ctx); ok {
		if a.Account != key {
			return nil, ErrAttemptChanged
		}
		return m.DoPool(ctx, a.Pool, req)
	}
	e, err := m.get(key)
	if err != nil {
		return nil, err
	}
	if e.config.Login == nil {
		return nil, ErrUnsupported
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
	if err = e.load(ctx); err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	retryable := method == http.MethodGet || method == http.MethodHead
	base := map[string]string{"Cookie": "", "Proxy-Authorization": ""}
	for h := range e.authHeaders {
		base[h] = ""
	}
	for k, v := range req.Headers {
		k = http.CanonicalHeaderKey(k)
		if e.authHeaders[k] || k == "Cookie" || k == "Proxy-Authorization" {
			return nil, errors.New("request cannot override account credentials")
		}
		base[k] = v
	}
	if e.config.UserAgent != "" {
		base["User-Agent"] = e.config.UserAgent
	}
	for attempt := 0; attempt < 2; attempt++ {
		c, version, err := e.ensure(ctx)
		if err != nil {
			return nil, err
		}
		r := req
		r.NoRedirect = true
		r.Headers = maps.Clone(base)
		for k, v := range c.Headers {
			r.Headers[k] = v
		}
		response, fetchErr := m.client.Do(htmlfetch.WithSession(ctx, c.Session), r)
		outcome := e.config.Classify(response, fetchErr)
		switch outcome {
		case Expired:
			m.Invalidate(key, version)
			if retryable && attempt == 0 {
				continue
			}
			return response, ErrExpired
		case Forbidden:
			return response, ErrForbidden
		case Challenge:
			e.mu.Lock()
			if e.version == version {
				e.paused = true
				e.revision++
				e.failure = ErrChallenge
			}
			e.mu.Unlock()
			return response, ErrChallenge
		case Valid, Unknown:
		default:
			return response, errors.New("unknown account response classification")
		}
		err = e.observe(ctx, version, response, outcome == Valid && fetchErr == nil)
		if err != nil {
			return response, err
		}
		return response, fetchErr
	}
	return nil, ErrExpired
}

func (e *entry) observe(ctx context.Context, version uint64, response *htmlfetch.Response, valid bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.version != version || e.paused {
		return nil
	}
	if valid && e.config.Update != nil {
		candidate := cloneCredentials(e.current)
		if err := e.config.Update(candidate, response); err != nil {
			return &Failure{Code: "credential-update-failed", Cause: err}
		}
		if err := e.checkCredentials(candidate); err != nil {
			return &Failure{Code: "invalid-credential-update", Cause: err}
		}
		if candidate.Session != e.current.Session {
			return errors.New("credential Update cannot replace CookieJar")
		}
		if !reflect.DeepEqual(candidate.Headers, e.current.Headers) || !reflect.DeepEqual(candidate.Values, e.current.Values) {
			e.version++
			e.current = candidate
			e.seed = maps.Clone(candidate.Values)
		}
	}
	return e.saveLocked(ctx)
}

// FetchHTML uses the same account as Do. It intentionally bypasses Engine's raw
// HTML archive; export evidence only through explicit sanitized sample policies.
func (m *Manager) FetchHTML(ctx context.Context, key Key, rawURL string) (*htmlfetch.Page, error) {
	r, err := m.Do(ctx, key, htmlfetch.Request{URL: rawURL})
	if err != nil {
		return nil, err
	}
	p, err := htmlfetch.NewRenderedPage(r.URL.String(), string(r.Body))
	if err != nil {
		return nil, fmt.Errorf("parse account HTML: %w", err)
	}
	p.StatusCode = r.StatusCode
	p.ContentType = r.Header.Get("Content-Type")
	return p, nil
}
