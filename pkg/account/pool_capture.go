package account

import (
	"context"
	"net/url"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func (m *Manager) CapturePoolRequest(ctx context.Context, key PoolKey, rawURL string, opts browser.CaptureOptions) (*browser.CaptureResult, error) {
	_, a, err := m.poolAttempt(ctx, key, Browser)
	if err != nil {
		return nil, err
	}
	opts.Page, err = m.poolPageOptions(a, rawURL, opts.Page)
	if err != nil {
		return nil, err
	}
	ctx = WithAttempt(ctx, a)
	var result *browser.CaptureResult
	initial := opts.Page
	opts.Page.Block = nil
	opts.Page.GovernanceOrigins = nil
	err = m.withPoolBrowser(ctx, key, &initial, func(page *rod.Page) error {
		var err error
		result, err = browser.CaptureRequestPage(ctx, page, rawURL, opts)
		return err
	})
	return result, err
}

func (m *Manager) CapturePoolResponse(ctx context.Context, key PoolKey, rawURL string, opts browser.ResponseOptions) (*browser.CapturedResponse, error) {
	_, a, err := m.poolAttempt(ctx, key, Browser)
	if err != nil {
		return nil, err
	}
	opts.Page, err = m.poolPageOptions(a, rawURL, opts.Page)
	if err != nil {
		return nil, err
	}
	ctx = WithAttempt(ctx, a)
	var result *browser.CapturedResponse
	initial := opts.Page
	opts.Page.Block = nil
	opts.Page.GovernanceOrigins = nil
	err = m.withPoolBrowser(ctx, key, &initial, func(page *rod.Page) error {
		var err error
		result, err = browser.CaptureResponsePage(ctx, page, rawURL, opts)
		if err != nil || result == nil {
			return err
		}
		e, _ := m.get(a.Account)
		capturedOrigin, originErr := origin(result.Request.URL)
		if originErr != nil || !e.origins[capturedOrigin] {
			return nil
		}
		u, _ := url.Parse(result.Request.URL)
		outcome := e.config.Classify(&htmlfetch.Response{URL: u, StatusCode: result.StatusCode, Header: result.Header, Body: result.Body}, nil)
		switch outcome {
		case Expired:
			return ErrExpired
		case Challenge:
			return ErrChallenge
		case Forbidden:
			return ErrForbidden
		}
		return nil
	})
	return result, err
}
