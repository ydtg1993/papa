package account

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"reflect"
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
	imported, err := browser.PageStateFromSession(candidate.Session, cfg.URL)
	if err != nil {
		return &Failure{Code: "session-import-failed", Cause: err}
	}
	if state != nil && state.Origin == imported.Origin {
		imported.LocalStorage = maps.Clone(state.LocalStorage)
	}
	if cfg.Import != nil {
		if err = cfg.Import(ctx, candidate, imported); err != nil {
			return &Failure{Code: "session-import-failed", Cause: err}
		}
	}
	if err = browser.ValidatePageState(imported); err != nil {
		return &Failure{Code: "session-import-failed", Cause: err}
	}
	if imported.Origin != originOf(cfg.URL) {
		return errors.New("session import changed browser origin")
	}
	var saved *browser.PageState
	err = useBrowser(ctx, &cfg, imported, func(page *rod.Page) error {
		valid, checkErr := cfg.Check(ctx, page)
		if checkErr != nil {
			return &Failure{Code: "bridge-browser-check-failed", Cause: checkErr}
		}
		if !valid {
			return ErrExpired
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
		return &Failure{Code: "bridge-http-check-failed", Cause: err}
	}
	if !valid {
		return ErrExpired
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused || e.version != version || e.revision != revision || e.browserRevision != browserRevision {
		return ErrSuperseded
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
	if !reflect.DeepEqual(old.Headers, candidate.Headers) || !reflect.DeepEqual(old.Values, candidate.Values) {
		e.version++
	}
	e.lastCheck, e.browserCheck = time.Now(), time.Now()
	e.browserRevision++
	if err = e.saveLocked(ctx); err != nil {
		return err
	}
	committed = true
	return nil
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
