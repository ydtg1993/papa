package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// ResponseOptions captures the first matching response, including non-2xx responses.
// Match must be fast and must not call browser methods. Body is absent during Match.
type ResponseOptions struct {
	// MaxRequestBodySize defaults to 64 KiB; event bodies may also be omitted by Chrome.
	MaxRequestBodySize int64
	Page               PageOptions
	Match              func(CapturedResponse) bool
	Timeout            time.Duration
	MaxBodySize        int64
}

// CapturedResponse is an in-memory snapshot; headers/body can contain credentials.
// Request.Method may be empty if its request event was unavailable.
type CapturedResponse struct {
	RequestHeader         http.Header
	RequestBody           []byte
	RequestBodyIncomplete bool
	Request               CapturedRequest
	StatusCode            int
	Header                http.Header
	Body                  []byte
}

// CaptureResponse observes one response through completion. This is for bounded
// API responses, not downloads or streams. MaxBodySize limits the returned body;
// CDP retrieves complete bodies, so it is not a hard Chrome memory limit.
func (b *Browser) CaptureResponse(ctx context.Context, pageURL string, opts ResponseOptions) (*CapturedResponse, error) {
	if err := validateNavigation(pageURL); err != nil {
		return nil, err
	}
	if opts.Match == nil {
		return nil, fmt.Errorf("capture response: Match is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultCaptureTimeout
	}
	if opts.MaxBodySize <= 0 {
		opts.MaxBodySize = 10 << 20
	}
	if opts.MaxRequestBodySize <= 0 {
		opts.MaxRequestBodySize = 64 << 10
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	page, err := b.newPageBaseContext(ctx, opts.Page)
	if err != nil {
		return nil, err
	}
	// Cleanup must survive timeout/cancellation of the capture itself.
	defer func() { _ = page.Context(context.WithoutCancel(ctx)).Timeout(3 * time.Second).Close() }()
	page = page.Context(ctx)
	type outcome struct {
		response *CapturedResponse
		err      error
	}
	done := make(chan outcome, 1)
	requests := make(map[proto.NetworkRequestID]*CapturedResponse)
	var selected *CapturedResponse
	var selectedID proto.NetworkRequestID
	wait := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			if _, exists := requests[e.RequestID]; exists || len(requests) < 256 {
				r := &CapturedResponse{Request: CapturedRequest{URL: e.Request.URL, Method: e.Request.Method, ResourceType: e.Type}, RequestHeader: make(http.Header)}
				for key, value := range e.Request.Headers {
					r.RequestHeader.Set(key, value.Str())
				}
				raw := e.Request.PostData
				r.RequestBodyIncomplete = e.Request.HasPostData && raw == ""
				if int64(len(raw)) > opts.MaxRequestBodySize {
					raw = raw[:opts.MaxRequestBodySize]
					r.RequestBodyIncomplete = true
				}
				r.RequestBody = []byte(raw)
				requests[e.RequestID] = r
			}
		},
		func(e *proto.NetworkResponseReceived) bool {
			if selected != nil {
				return false
			}
			r := requests[e.RequestID]
			if r == nil {
				r = &CapturedResponse{RequestBodyIncomplete: true}
			}
			r.Request.URL, r.Request.ResourceType = e.Response.URL, e.Type
			r.StatusCode, r.Header = e.Response.Status, make(http.Header)
			for key, value := range e.Response.Headers {
				r.Header.Set(key, value.Str())
			}
			if !opts.Match(*r) {
				return false
			}
			selected, selectedID = r, e.RequestID
			return false
		},
		func(e *proto.NetworkLoadingFinished) bool {
			delete(requests, e.RequestID)
			if selected == nil || e.RequestID != selectedID {
				return false
			}
			result, err := (proto.NetworkGetResponseBody{RequestID: selectedID}).Call(page)
			if err == nil {
				if result.Base64Encoded {
					selected.Body, err = base64.StdEncoding.DecodeString(result.Body)
				} else {
					selected.Body = []byte(result.Body)
				}
				if int64(len(selected.Body)) > opts.MaxBodySize {
					selected.Body = nil
					err = &htmlfetch.BodyTooLargeError{MaxBodySize: opts.MaxBodySize}
				}
			}
			done <- outcome{selected, err}
			return true
		},
		func(e *proto.NetworkLoadingFailed) bool {
			delete(requests, e.RequestID)
			if selected == nil || e.RequestID != selectedID {
				return false
			}
			done <- outcome{selected, fmt.Errorf("capture response: loading failed: %s", e.ErrorText)}
			return true
		},
	)
	go wait()
	nav := make(chan error, 1)
	go func() {
		navigation := page
		if opts.Page.Timeout > 0 {
			navigation = page.Timeout(opts.Page.Timeout)
		}
		nav <- navigation.Navigate(pageURL)
	}()
	for {
		select {
		case out := <-done:
			return out.response, out.err
		case err := <-nav:
			if err != nil {
				return nil, fmt.Errorf("capture response navigation: %w", err)
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("capture response: %w", ctx.Err())
		}
	}
}
