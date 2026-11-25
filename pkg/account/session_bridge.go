package account

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser"
)

// withSessionBrowser owns one ordered exchange. Only the non-pool path may
// authenticate; pool attempts never log in, change accounts or replay callbacks.
func (m *Manager) withSessionBrowser(ctx context.Context, e *entry, a *Attempt, options *browser.PageOptions, fn func(*rod.Page) error) error {
	if err := e.enter(ctx); err != nil {
		return err
	}
	defer e.leave()
	if err := e.load(ctx); err != nil {
		return err
	}
	if err := e.validateIdentityState(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	httpCtx, err := e.context(ctx)
	if err != nil {
		return err
	}
	if a == nil {
		if _, _, err = e.ensure(httpCtx); err != nil {
			return err
		}
	} else if err = a.owner.ValidateAttempt(a); err != nil {
		return err
	}
	if err = e.enterBrowser(ctx); err != nil {
		return err
	}
	defer func() { <-e.browserSlot }()
	e.mu.Lock()
	candidate, err := detachedCandidate(e.current)
	state, stateErr := clonePageState(e.pageState)
	version, revision, browserRevision := e.version, e.revision, e.browserRevision
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if stateErr != nil {
		return stateErr
	}
	if candidate == nil || (a != nil && version != a.CredentialVersion) {
		return ErrAttemptChanged
	}
	cfg := *e.config.Browser
	if options != nil {
		if options.Block != nil {
			cfg.Options.Block = options.Block
		}
		cfg.Options.GovernanceOrigins = append(append([]string(nil), cfg.Options.GovernanceOrigins...), options.GovernanceOrigins...)
	}
	imported, err := importSession(ctx, candidate, state, &cfg)
	if err != nil {
		return err
	}
	var saved *browser.PageState
	err = useBrowser(ctx, &cfg, imported, func(page *rod.Page) error {
		valid, checkErr := cfg.Check(ctx, page)
		if checkErr != nil {
			return &sessionCheckFailure{Browser, &Failure{Code: "bridge-browser-check-failed", Cause: checkErr}}
		}
		if !valid {
			return &sessionCheckFailure{Browser, ErrExpired}
		}
		if err := browserOrigin(page, cfg.URL); err != nil {
			return err
		}
		if a != nil {
			if err := a.owner.ValidateAttempt(a); err != nil {
				return err
			}
		}
		e.mu.Lock()
		stale := e.paused || e.version != version || e.revision != revision || e.browserRevision != browserRevision
		e.mu.Unlock()
		if stale {
			return ErrSuperseded
		}
		if err := m.beforeOperation(ctx, e.key); err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
		if err := browserOrigin(page, cfg.URL); err != nil {
			return err
		}
		var err error
		saved, err = exportSession(ctx, page, &cfg, candidate)
		return err
	})
	if err != nil {
		return err
	}
	valid, err := e.config.Check(httpCtx, candidate)
	if err != nil {
		return &sessionCheckFailure{HTTP, &Failure{Code: "bridge-http-check-failed", Cause: err}}
	}
	if !valid {
		return &sessionCheckFailure{HTTP, ErrExpired}
	}
	if err = e.checkCredentials(candidate); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if a != nil {
		if err = a.owner.ValidateAttempt(a); err != nil {
			return err
		}
	}
	_, _, err = e.commitSession(ctx, candidate, saved, version, revision, browserRevision, true)
	return err
}

func (e *entry) sessionBridge() bool {
	return e.config.Browser != nil && e.config.Browser.SessionBridge
}

// A detached candidate's failure identifies the transport needing a probe,
// without proving that the previous committed credentials are invalid.
type sessionCheckFailure struct {
	transport Transport
	cause     error
}

func (e *sessionCheckFailure) Error() string { return "account session verification failed" }
func (e *sessionCheckFailure) Unwrap() error { return e.cause }
func sessionFailureTransport(err error, fallback Transport) Transport {
	var failure *sessionCheckFailure
	if errors.As(err, &failure) {
		return failure.transport
	}
	return fallback
}

// Capture under e.mu; restoration also runs under e.mu. Detached verification
// must never destroy the last committed session when Store.Save fails/panics.
func (e *entry) sessionRollbackLocked() func() {
	c, p, s := e.current, e.pageState, e.seed
	v, bv := e.version, e.browserRevision
	check, login, bcheck := e.lastCheck, e.lastLogin, e.browserCheck
	failure, retry := e.failure, e.retryAt
	return func() {
		e.current, e.pageState, e.seed = c, p, s
		e.version, e.browserRevision = v, bv
		e.lastCheck, e.lastLogin, e.browserCheck = check, login, bcheck
		e.failure, e.retryAt = failure, retry
	}
}

// Cookies have one authority: the latest HTTP jar. The browser snapshot retains
// only its origin-scoped storage; checks may have rotated cookies since export.
func sessionPageState(c *Credentials, state *browser.PageState, cfg *BrowserConfig) (*browser.PageState, error) {
	result, err := browser.PageStateFromSession(c.Session, cfg.URL)
	if err != nil {
		return nil, err
	}
	if state != nil && state.Origin == result.Origin {
		result.LocalStorage = maps.Clone(state.LocalStorage)
	}
	if result.LocalStorage == nil {
		result.LocalStorage = map[string]string{}
	}
	return result, nil
}

func importSession(ctx context.Context, c *Credentials, state *browser.PageState, cfg *BrowserConfig) (*browser.PageState, error) {
	imported, err := sessionPageState(c, state, cfg)
	if err == nil && cfg.Import != nil {
		err = cfg.Import(ctx, c, imported)
	}
	if err == nil {
		err = browser.ValidatePageState(imported)
	}
	if err != nil {
		return nil, &Failure{Code: "session-import-failed", Cause: err}
	}
	if imported.Origin != originOf(cfg.URL) {
		return nil, errors.New("session import changed browser origin")
	}
	return imported, nil
}

// Caller holds the shared account permit. The lifecycle CAS also protects
// against Logout/Pause, which deliberately do not wait for an active exchange.
func (e *entry) commitSession(ctx context.Context, candidate *Credentials, saved *browser.PageState, version, revision, browserRevision uint64, browserChecked bool) (v, bv uint64, err error) {
	if err = ctx.Err(); err != nil {
		return version, browserRevision, err
	}
	if err = e.checkCredentials(candidate); err != nil {
		return version, browserRevision, err
	}
	if saved != nil {
		saved, err = sessionPageState(candidate, saved, e.config.Browser)
		if err != nil {
			return version, browserRevision, err
		}
	}
	candidate.BrowserState = saved
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused || e.version != version || e.revision != revision || e.browserRevision != browserRevision {
		return version, browserRevision, ErrSuperseded
	}
	old, oldState, oldSeed := e.current, e.pageState, e.seed
	oldCheck, oldBrowserCheck := e.lastCheck, e.browserCheck
	committed := false
	defer func() {
		if !committed {
			e.current, e.pageState, e.seed = old, oldState, oldSeed
			e.lastCheck, e.browserCheck = oldCheck, oldBrowserCheck
			e.version, e.browserRevision = version, browserRevision
		}
	}()
	e.current, e.pageState, e.seed = candidate, saved, maps.Clone(candidate.Values)
	// Cookie rotation follows ordinary HTTP semantics. Explicit token/header
	// changes invalidate older attempts just as Config.Update does.
	if !maps.Equal(old.Headers, candidate.Headers) || !maps.Equal(old.Values, candidate.Values) {
		e.version++
	}
	e.lastCheck = time.Now()
	if browserChecked {
		e.browserCheck = e.lastCheck
	}
	e.browserRevision++
	if err = e.saveLocked(ctx); err != nil {
		return version, browserRevision, err
	}
	committed = true
	return e.version, e.browserRevision, nil
}

func originOf(raw string) string { value, _ := origin(raw); return value }

// All shared-session paths (Rod login, OA candidate and ordinary browser use)
// use the same scoped export. HTTP verification remains the caller's next step.
func exportSession(ctx context.Context, page *rod.Page, cfg *BrowserConfig, candidate *Credentials) (*browser.PageState, error) {
	if err := browserOrigin(page, cfg.URL); err != nil {
		return nil, err
	}
	saved, err := browser.ExportPageState(page)
	if err != nil {
		return nil, err
	}
	all, err := (proto.StorageGetCookies{BrowserContextID: page.Browser().BrowserContextID}).Call(page.Browser().Context(ctx))
	if err != nil {
		return nil, err
	}
	saved.Cookies = nil
	u, _ := url.Parse(saved.Origin)
	for _, cookie := range all.Cookies {
		if cookie == nil {
			return nil, errors.New("invalid browser cookie")
		}
		domain := strings.TrimPrefix(cookie.Domain, ".")
		if domain == u.Hostname() || (strings.HasPrefix(cookie.Domain, ".") && strings.HasSuffix(u.Hostname(), "."+domain)) {
			saved.Cookies = append(saved.Cookies, cookie)
		}
	}
	candidate.Session, err = browser.SessionWithPageState(candidate.Session, saved)
	if err != nil {
		return nil, err
	}
	if cfg.Export != nil {
		if candidate.Headers == nil {
			candidate.Headers = map[string]string{}
		}
		if candidate.Values == nil {
			candidate.Values = map[string]string{}
		}
		if err = cfg.Export(ctx, page, candidate); err != nil {
			return nil, &Failure{Code: "session-export-failed", Cause: err}
		}
	}
	return saved, browserOrigin(page, cfg.URL)
}
