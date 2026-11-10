package browser

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

// WithStatePage restores state before the first request and site script, then
// calls fn with the loaded page. It requires an isolated browser context; callers
// must use a fresh context for each account and discard it on any restore error.
// Cookies are merged with PageOptions cookies (state wins); existing context
// cookies are not cleared. A nil state starts without restoring credentials,
// while retaining the same target-origin checks. URLs with userinfo are rejected.
//
// LocalStorage is replaced once, only in the matching main-frame origin. A
// page-scoped CDP handshake removes the initializer before site scripts run;
// completion does not depend on site-owned storage. Redirects and later
// navigation therefore preserve site updates. A different
// final origin is rejected, but redirects and normal cookie domain/path rules
// still apply: this helper is not a network sandbox.
//
// Timeout (default 30s) bounds setup, navigation, and waits together. Interact and
// fn use ctx and must cooperate with cancellation. The page is closed even after
// errors, cancellation, or panic; neither the page nor browser may escape its
// enclosing WithIsolatedBrowser scope.
func (b *Browser) WithStatePage(ctx context.Context, rawURL string, state *PageState, opts PageOptions, fn func(*rod.Page) error) (err error) {
	if err := validateNavigation(rawURL); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("state page: callback required")
	}
	if b == nil || b.Browser == nil || b.Browser.BrowserContextID == "" {
		return fmt.Errorf("page state requires an isolated browser context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("state page requires an HTTP URL")
	}
	if u.User != nil {
		return fmt.Errorf("state page URL must not contain userinfo")
	}
	if state != nil {
		if err := validatePageState(state); err != nil {
			return err
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	navCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	page, err := b.newPageBaseContext(navCtx, opts)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if closeErr := page.Context(cleanup).Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close state page: %w", closeErr))
		}
	}()
	challenge, stopChallenge := observeSiteChallenge(page)
	defer stopChallenge()
	defer func() { err = challenge.prefer(err) }()
	// Use the browser's URL parser before navigation, on the untouched blank
	// page. This matches location.origin for default ports, IDN, and IPv6 too.
	origin, err := stateNavigationOrigin(page, rawURL)
	if err != nil {
		return err
	}
	if state != nil {
		stateOrigin, err := stateNavigationOrigin(page, state.Origin)
		if err != nil {
			return err
		}
		if stateOrigin != origin {
			return fmt.Errorf("page state origin mismatch")
		}
		normalized := *state
		normalized.Origin = origin
		state = &normalized
	}

	var initialized <-chan error
	var cleanupInitializer func() error
	if state != nil {
		if err := page.SetCookies(stateNavigationCookies(state)); err != nil {
			return fmt.Errorf("restore state cookies: %w", err)
		}
		initialized, cleanupInitializer, err = installStateInitializer(page, state)
		if err != nil {
			return err
		}
		defer func() {
			if cleanupInitializer != nil {
				err = errors.Join(err, cleanupInitializer())
			}
		}()
	}
	if err := page.Navigate(rawURL); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := waitStatePageLoad(page); err != nil {
		return fmt.Errorf("wait load: %w", err)
	}
	if err := challenge.prefer(nil); err != nil {
		return err
	}
	if err := checkStatePageOrigin(page, origin); err != nil {
		return err
	}
	if state != nil {
		select {
		case err := <-initialized:
			if err != nil {
				return err
			}
		case <-navCtx.Done():
			return navCtx.Err()
		}
		// Join the observer and disable its debugger before any user waits or
		// callbacks. The deferred cleanup handles every earlier failure path.
		cleanupErr := cleanupInitializer()
		cleanupInitializer = nil
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	if err := applyWaits(page, opts); err != nil {
		return err
	}
	if opts.Wait > 0 {
		timer := time.NewTimer(opts.Wait)
		defer timer.Stop()
		select {
		case <-navCtx.Done():
			return navCtx.Err()
		case <-timer.C:
		}
	}
	if err := navCtx.Err(); err != nil {
		return err
	}
	if err := checkStatePageOrigin(page, origin); err != nil {
		return err
	}
	page = page.Context(ctx)
	if opts.Interact != nil {
		if err := opts.Interact(ctx, page); err != nil {
			return fmt.Errorf("interact: %w", err)
		}
	}
	if err := challenge.prefer(nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkStatePageOrigin(page, origin); err != nil {
		return err
	}
	return fn(page)
}

func waitStatePageLoad(page *rod.Page) error {
	for {
		err := page.WaitLoad()
		// An immediate client redirect destroys the promise used by Rod's
		// WaitLoad. Continue waiting in the replacement document, under navCtx.
		if !stateLoadContextReplaced(err) {
			return err
		}
		if err := page.GetContext().Err(); err != nil {
			return err
		}
	}
}

func stateNavigationOrigin(page *rod.Page, rawURL string) (string, error) {
	result, err := page.Eval(`(raw) => {
		try {
			const target = new URL(raw);
			if ((target.protocol !== 'http:' && target.protocol !== 'https:') || target.username || target.password) return '';
			return target.origin;
		} catch (_) { return ''; }
	}`, rawURL)
	if err != nil {
		return "", fmt.Errorf("parse state page origin: %w", err)
	}
	if origin := result.Value.Str(); origin != "" && origin != "null" {
		return origin, nil
	}
	return "", fmt.Errorf("state page requires an HTTP URL without userinfo")
}

func checkStatePageOrigin(page *rod.Page, origin string) error {
	result, err := page.Eval(`() => location.origin`)
	if err != nil {
		return fmt.Errorf("read state page origin: %w", err)
	}
	if result.Value.Str() != origin {
		return fmt.Errorf("page state origin mismatch")
	}
	return nil
}

func stateNavigationCookies(state *PageState) []*proto.NetworkCookieParam {
	cookies := make([]*proto.NetworkCookieParam, 0, len(state.Cookies))
	for _, c := range state.Cookies {
		p := &proto.NetworkCookieParam{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite}
		if strings.HasPrefix(c.Domain, ".") {
			p.Domain = c.Domain
		} else {
			p.URL = state.Origin + c.Path
		}
		if !c.Session {
			p.Expires = c.Expires
		}
		cookies = append(cookies, p)
	}
	return cookies
}

// The initializer pauses its own isolated world after restoring storage. The
// controller removes the script before resuming, so even an immediate client
// redirect cannot run it twice. Only this page's CDP session enables Debugger.
func installStateInitializer(page *rod.Page, state *PageState) (<-chan error, func() error, error) {
	name := "papaRestore_" + rand.Text()
	source, err := stateNavigationScript(state, name)
	if err != nil {
		return nil, nil, err
	}
	script, err := (proto.PageAddScriptToEvaluateOnNewDocument{Source: source, WorldName: name}).Call(page)
	if err != nil {
		return nil, nil, fmt.Errorf("install state initializer: %w", err)
	}
	observer, cancel := page.WithCancel()
	result := make(chan error, 1)
	stopped := make(chan struct{})
	removed := false // read by cleanup only after the observer has stopped
	wait := observer.EachEvent(func(event *proto.DebuggerPaused) bool {
		if len(event.CallFrames) == 0 || event.CallFrames[0].FunctionName != name {
			_ = (proto.DebuggerResume{}).Call(observer)
			return false
		}
		var initErr error
		value, err := (proto.DebuggerEvaluateOnCallFrame{
			CallFrameID: event.CallFrames[0].CallFrameID, Expression: "restored", Silent: true, ReturnByValue: true,
		}).Call(observer)
		if err != nil {
			initErr = fmt.Errorf("verify state initializer: %w", err)
		} else if value.ExceptionDetails != nil || value.Result == nil || !value.Result.Value.Bool() {
			initErr = fmt.Errorf("initialize page local storage failed")
		}
		if err := (proto.PageRemoveScriptToEvaluateOnNewDocument{Identifier: script.Identifier}).Call(observer); err != nil {
			initErr = errors.Join(initErr, fmt.Errorf("remove state initializer: %w", err))
		} else {
			removed = true
		}
		if err := (proto.DebuggerResume{}).Call(observer); err != nil {
			initErr = errors.Join(initErr, fmt.Errorf("resume state page: %w", err))
		}
		result <- initErr
		return true
	})
	go func() {
		defer close(stopped)
		wait()
	}()
	cleanup := func() error {
		cancel()
		<-stopped
		ctx, stop := context.WithTimeout(context.WithoutCancel(page.GetContext()), 3*time.Second)
		defer stop()
		p := page.Context(ctx)
		var err error
		if !removed {
			err = (proto.PageRemoveScriptToEvaluateOnNewDocument{Identifier: script.Identifier}).Call(p)
		}
		// Disabling also releases a paused page if cancellation interrupted the
		// handshake; the enclosing WithStatePage then closes the page.
		err = errors.Join(err, (proto.DebuggerDisable{}).Call(p))
		if err != nil {
			return fmt.Errorf("clean up state initializer: %w", err)
		}
		return nil
	}
	return result, cleanup, nil
}

func stateNavigationScript(state *PageState, name string) (string, error) {
	data, err := json.Marshal(struct {
		Origin string            `json:"origin"`
		Values map[string]string `json:"values"`
	}{state.Origin, state.LocalStorage})
	if err != nil {
		return "", fmt.Errorf("encode state initializer failed")
	}
	// Parse JSON rather than embed an object literal, so a storage key named
	// __proto__ is data and does not change JavaScript object-literal semantics.
	quoted, _ := json.Marshal(string(data))
	return `(function ` + name + `(state) {
		if (window !== window.top || location.origin !== state.origin) return;
		let restored = false;
		try {
			localStorage.clear();
			for (const [key, value] of Object.entries(state.values || {})) {
				localStorage.setItem(key, value);
			}
			restored = true;
		} catch (_) {
			// The controller detects failure without exposing credentials
			// or an unhandled exception containing the initializer source.
		}
		debugger;
	})(JSON.parse(` + string(quoted) + `));`, nil
}

// Chromium also reports this transient navigation race with a different CDP
// message. Actual closed sessions return their own error on the next wait.
func stateLoadContextReplaced(err error) bool {
	return errors.Is(err, cdp.ErrCtxDestroyed) || errors.Is(err, &cdp.Error{Code: -32000, Message: "Inspected target navigated or closed"})
}
