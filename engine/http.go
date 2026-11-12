package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// FetchIncremental performs an account-scoped conditional GET. Persist the returned
// representation only after business validation/storage succeeds.
func (e *Engine) FetchIncremental(ctx context.Context, rawURL string, previous *htmlfetch.Representation) (*htmlfetch.IncrementalResult, error) {
	if a, ok := account.AttemptFromContext(ctx); ok {
		m := e.GetAccountManager()
		if m == nil {
			return nil, account.ErrPoolUnknown
		}
		scope, _ := json.Marshal(a.Account)
		if previous != nil && previous.Scope != string(scope) {
			previous = nil
		}
		result, err := htmlfetch.FetchIncrementalWith(ctx, rawURL, previous, func(ctx context.Context, req htmlfetch.Request) (*htmlfetch.Response, error) {
			return m.DoPool(ctx, a.Pool, req)
		})
		if result != nil {
			result.Representation.Scope = string(scope)
		}
		return result, err
	}
	if e.GetHTMLClient() == nil {
		return nil, fmt.Errorf("HTTP 客户端未启用：请开启 html.enable")
	}
	result, err := e.GetHTMLClient().FetchIncremental(ctx, rawURL, previous)
	data := map[string]any{}
	if result != nil {
		data["changed"] = result.Changed
		data["not_modified"] = result.NotModified
		data["status_code"] = result.Response.StatusCode
	}
	if err != nil {
		traceFromCtx(ctx).Fail("增量 HTTP 请求", fmt.Errorf("incremental HTTP exchange failed"), data)
	} else {
		traceFromCtx(ctx).Step("增量 HTTP 请求", data)
	}
	return result, err
}

// FetchHTTP performs an API/raw HTTP exchange using html configuration, proxy and
// context headers. No response bodies or credentials are automatically archived.
func (e *Engine) FetchHTTP(ctx context.Context, request htmlfetch.Request) (*htmlfetch.Response, error) {
	if a, ok := account.AttemptFromContext(ctx); ok {
		return e.FetchPoolHTTP(ctx, a.Pool, request)
	}
	if e.GetHTMLClient() == nil {
		return nil, fmt.Errorf("HTTP 客户端未启用：请开启 html.enable")
	}
	response, err := e.GetHTMLClient().Do(ctx, request)
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	data := map[string]any{"method": method}
	if response != nil {
		data["status_code"] = response.StatusCode
		data["body_bytes"] = len(response.Body)
	}
	// Do not copy transport error strings into trace: they can contain signed URLs.
	if err != nil {
		traceFromCtx(ctx).Fail("HTTP 请求", fmt.Errorf("HTTP exchange failed"), data)
	} else {
		traceFromCtx(ctx).Step("HTTP 请求", data)
	}
	return response, err
}

// CaptureResponse borrows a browser and captures a bounded API response.
// The returned raw data belongs to the caller; trace records metadata only.
func (e *Engine) CaptureResponse(ctx context.Context, pageURL string, opts browser.ResponseOptions) (*browser.CapturedResponse, error) {
	if a, ok := account.AttemptFromContext(ctx); ok {
		m := e.GetAccountManager()
		if m == nil {
			return nil, account.ErrPoolUnknown
		}
		opts.Page = mergeBrowserOptions(ctx, opts.Page)
		response, err := m.CapturePoolResponse(ctx, a.Pool, pageURL, opts)
		data := map[string]any{}
		if response != nil {
			data["status_code"] = response.StatusCode
			data["body_bytes"] = len(response.Body)
		}
		if err != nil {
			traceFromCtx(ctx).Fail("捕获 HTTP 响应", fmt.Errorf("account response capture failed"), data)
		} else {
			traceFromCtx(ctx).Step("捕获 HTTP 响应", data)
		}
		return response, err
	}
	if err := browserProxyError(ctx); err != nil {
		return nil, err
	}
	if e.GetBrowserPool() == nil {
		return nil, fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	opts.Page = mergeBrowserOptions(ctx, opts.Page)
	var response *browser.CapturedResponse
	err := e.GetBrowserPool().WithBrowser(ctx, func(b *browser.Browser) error {
		var err error
		response, err = b.CaptureResponse(ctx, pageURL, opts)
		return err
	})
	data := map[string]any{}
	if response != nil {
		data["status_code"] = response.StatusCode
		data["body_bytes"] = len(response.Body)
	}
	if err != nil {
		traceFromCtx(ctx).Fail("捕获 HTTP 响应", fmt.Errorf("response capture failed"), data)
	} else {
		traceFromCtx(ctx).Step("捕获 HTTP 响应", data)
	}
	return response, err
}
