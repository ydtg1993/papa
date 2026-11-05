package account

import (
	"context"
	"errors"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/browser"
)

func useBrowser(ctx context.Context, cfg *BrowserConfig, state *browser.PageState, fn func(*rod.Page) error) error {
	use := cfg.Pool.WithIsolatedBrowser
	if cfg.Direct {
		use = cfg.Pool.WithDirectIsolatedBrowser
	}
	return use(ctx, func(b *browser.Browser) error { return b.WithStatePage(ctx, cfg.URL, state, cfg.Options, fn) })
}

func browserOrigin(page *rod.Page, rawURL string) error {
	r, err := page.Eval(`(url) => location.origin === new URL(url).origin`, rawURL)
	if err != nil {
		return &Failure{Code: "browser-origin-check-failed", Cause: err}
	}
	if !r.Value.Bool() {
		return errors.New("account browser left bootstrap origin")
	}
	return nil
}

// When HTTP Check and Browser are supplied without HTTP Login, authenticate in
// Rod and copy configured URL cookies. Export optionally supplies token headers.
func browserLogin(cfg BrowserConfig) func(context.Context, *Credentials) error {
	return func(ctx context.Context, c *Credentials) error {
		return useBrowser(ctx, &cfg, nil, func(page *rod.Page) error {
			if err := cfg.Login(ctx, page); err != nil {
				return err
			}
			if err := browserOrigin(page, cfg.URL); err != nil {
				return err
			}
			valid, err := cfg.Check(ctx, page)
			if err != nil {
				return err
			}
			if !valid {
				return ErrExpired
			}
			if err := browserOrigin(page, cfg.URL); err != nil {
				return err
			}
			urls := cfg.CookieURLs
			if len(urls) == 0 {
				urls = []string{cfg.URL}
			}
			for _, raw := range urls {
				if err := browser.CopyCookiesToSession(page, c.Session, raw); err != nil {
					return err
				}
			}
			if cfg.Export != nil {
				if err := cfg.Export(ctx, page, c); err != nil {
					return err
				}
			}
			if err := browserOrigin(page, cfg.URL); err != nil {
				return err
			}
			c.BrowserState, err = browser.ExportPageState(page)
			return err
		})
	}
}

var errRestoreExpired = errors.New("restored browser login expired")

// WithBrowser serializes browser operations for this account. Each operation
// restores state into a fresh context and verifies it. Expired restored state
// gets one clean login. The business callback is never replayed. State is saved
// only after callback success. HTTP credentials are not silently overwritten.
func (m *Manager) WithBrowser(ctx context.Context, key Key, fn func(*rod.Page) error) (err error) {
	if core.ProxyURLFrom(ctx) != "" {
		return errors.New("account browser requires a pool-bound proxy, not a request proxy")
	}
	if fn == nil {
		return errors.New("account browser callback required")
	}
	e, err := m.get(key)
	if err != nil {
		return err
	}
	if e.config.Browser == nil {
		return ErrUnsupported
	}
	if err = e.enter(ctx); err != nil {
		return err
	}
	defer e.leave()
	select {
	case e.browserSlot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.browserSlot }()
	if err = e.load(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	if e.paused {
		e.mu.Unlock()
		return ErrPaused
	}
	if e.failure != nil && time.Now().Before(e.retryAt) {
		err = e.failure
		e.mu.Unlock()
		return err
	}
	state, err := clonePageState(e.pageState)
	revision := e.revision
	e.mu.Unlock()
	if err != nil {
		return &Failure{Code: "invalid-browser-state", Cause: err}
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	var saved *browser.PageState
	loggedIn := false
	for attempt := 0; attempt < 2; attempt++ {
		err = useBrowser(ctx, e.config.Browser, state, func(page *rod.Page) error {
			valid, err := e.config.Browser.Check(ctx, page)
			if err != nil {
				return &Failure{Code: "browser-check-failed", Cause: err}
			}
			if err := browserOrigin(page, e.config.Browser.URL); err != nil {
				return err
			}
			if !valid {
				if state != nil {
					return errRestoreExpired
				}
				if err = e.config.Browser.Login(ctx, page); err != nil {
					return &Failure{Code: "browser-login-failed", Cause: err}
				}
				if err := browserOrigin(page, e.config.Browser.URL); err != nil {
					return err
				}
				valid, err = e.config.Browser.Check(ctx, page)
				if err != nil {
					return &Failure{Code: "browser-verification-failed", Cause: err}
				}
				if !valid {
					return ErrExpired
				}
				if err := browserOrigin(page, e.config.Browser.URL); err != nil {
					return err
				}
				loggedIn = true
			}
			if err = fn(page); err != nil {
				return err
			}
			if err := browserOrigin(page, e.config.Browser.URL); err != nil {
				return err
			}
			saved, err = browser.ExportPageState(page)
			if err != nil {
				return err
			}
			if !e.origins[saved.Origin] {
				return errors.New("browser callback left account origins")
			}
			return nil
		})
		if errors.Is(err, errRestoreExpired) {
			state = nil
			continue
		}
		break
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revision != revision || e.paused {
		return ErrSuperseded
	}
	if err != nil {
		// Only authentication failures cool down the account, not arbitrary parse
		// errors from the business callback. Expiry drops the browser snapshot.
		var failure *Failure
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && (errors.Is(err, ErrExpired) || errors.Is(err, ErrChallenge) || errors.As(err, &failure)) {
			e.pageState = nil
			e.failure = err
			e.retryAt = time.Now().Add(e.config.FailureCooldown)
			if errors.Is(err, ErrChallenge) {
				e.paused = true
			}
			e.revision++
		}
		return err
	}
	e.pageState = saved
	e.browserCheck = time.Now()
	if loggedIn {
		e.lastLogin = e.browserCheck
	}
	e.revision++
	e.failure = nil
	e.retryAt = time.Time{}
	return e.saveLocked(ctx)
}
