package engine

import (
	"context"
	"errors"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// SetAccountManager installs a caller-configured registry. Call before workers
// start; replacement does not cancel operations using the previous registry.
func (e *Engine) SetAccountManager(m *account.Manager) { e.fetchResources.SetAccount(m) }
func (e *Engine) GetAccountManager() *account.Manager  { return e.fetchResources.Account() }

// AccountStatus is a credential-free snapshot suitable for custom admin pages.
func (e *Engine) AccountStatus() []account.Status {
	m := e.GetAccountManager()
	if m == nil {
		return []account.Status{}
	}
	return m.Status()
}
func (e *Engine) FetchAccountHTTP(ctx context.Context, key account.Key, req htmlfetch.Request) (*htmlfetch.Response, error) {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return nil, errors.New("account manager is not configured")
	}
	r, err := m.Do(ctx, key, req)
	data := map[string]any{}
	if r != nil {
		data["status_code"] = r.StatusCode
		data["body_bytes"] = len(r.Body)
	}
	if err != nil {
		traceFrameworkFailure(ctx, traceFromCtx(ctx), err, "账号 HTTP 请求", errors.New("account HTTP exchange failed"), data)
	} else {
		traceFromCtx(ctx).Step("账号 HTTP 请求", data)
	}
	return r, err
}

// FetchAccountHTML does not register credential-bearing HTML in the raw archive.
func (e *Engine) FetchAccountHTML(ctx context.Context, key account.Key, rawURL string) (*htmlfetch.Page, error) {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return nil, errors.New("account manager is not configured")
	}
	return m.FetchHTML(ctx, key, rawURL)
}
func (e *Engine) WithAccountBrowser(ctx context.Context, key account.Key, fn func(*rod.Page) error) error {
	ctx = e.RuntimeContext(ctx, key.Site)
	m := e.GetAccountManager()
	if m == nil {
		return errors.New("account manager is not configured")
	}
	return m.WithBrowser(ctx, key, fn)
}
