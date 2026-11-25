package account

import (
	"context"
	"errors"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

// checkSessionTransport runs under the account exchange permit. A probe can
// rotate credentials too: successful checks must publish their detached state.
// No Login or business callback is executed here.
func (e *entry) checkSessionTransport(ctx context.Context, t Transport, expected, expectedBrowser uint64) (v, bv uint64, err error) {
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	e.mu.Lock()
	v, bv, revision := e.version, e.browserRevision, e.revision
	if (expected != 0 && expected != v) || (expectedBrowser != 0 && expectedBrowser != bv) {
		e.mu.Unlock()
		return v, bv, ErrSuperseded
	}
	if e.current == nil {
		e.mu.Unlock()
		return v, bv, &sessionCheckFailure{HTTP, ErrExpired}
	}
	c, err := detachedCandidate(e.current)
	state, stateErr := clonePageState(e.pageState)
	e.mu.Unlock()
	if err != nil {
		return v, bv, err
	}
	if stateErr != nil {
		return v, bv, stateErr
	}
	httpCtx, err := e.context(ctx)
	if err != nil {
		return v, bv, err
	}
	checkHTTP := func() error {
		valid, err := e.config.Check(httpCtx, c)
		if err != nil {
			status, _ := htmlfetch.StatusCode(err)
			kind := proxy.ClassifyHTTP(e.config.ProxyURL, status, err)
			if kind == proxy.TransportFailure || kind == proxy.AuthenticationFailure || errors.Is(err, proxy.ErrUnavailable) {
				return &Failure{Code: "fixed-proxy-unavailable", Cause: errors.Join(ErrFixedProxyUnavailable, err)}
			}
			return &Failure{Code: "existing-check-failed", Cause: err}
		}
		if !valid {
			return ErrExpired
		}
		return nil
	}
	if err = checkHTTP(); err != nil {
		return v, bv, &sessionCheckFailure{HTTP, err}
	}
	browserChecked := t == Browser || t == Both
	if browserChecked {
		if err = e.enterBrowser(ctx); err != nil {
			return v, bv, err
		}
		defer func() { <-e.browserSlot }()
		state, err = importSession(ctx, c, state, e.config.Browser)
		if err != nil {
			return v, bv, err
		}
		err = useBrowser(ctx, e.config.Browser, state, func(page *rod.Page) error {
			valid, err := e.config.Browser.Check(ctx, page)
			if err != nil {
				return &Failure{Code: "existing-browser-check-failed", Cause: err}
			}
			if !valid {
				return ErrExpired
			}
			state, err = exportSession(ctx, page, e.config.Browser, c)
			return err
		})
		if err != nil {
			return v, bv, &sessionCheckFailure{Browser, err}
		}
		if err = checkHTTP(); err != nil {
			return v, bv, &sessionCheckFailure{HTTP, err}
		}
	}
	return e.commitSession(ctx, c, state, v, revision, bv, browserChecked)
}
