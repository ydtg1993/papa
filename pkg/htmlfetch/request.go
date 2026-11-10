package htmlfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
)

// Request describes a bounded HTTP exchange. Empty Method means GET.
// Headers override configuration and context headers; empty values remove headers.
// Body is replayable for HTTP 307/308 redirects. Do adds no retry loop;
// standard net/http transport retry and redirect behavior still applies.
type Request struct {
	// NoRedirect returns 3xx to the caller instead of forwarding request headers.
	NoRedirect bool
	URL        string
	Method     string
	Headers    map[string]string
	Body       []byte
}

// Response owns its bounded body and headers. URL is the final URL after redirects.
// A non-2xx response is returned together with a StatusError.
type Response struct {
	URL        *url.URL
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Do performs an HTTP request using the same configuration and proxy as Fetch.
// It closes the network body before returning. Bodies larger than MaxBodySize
// are discarded; response metadata is still returned with BodyTooLargeError.
func (c *Client) Do(ctx context.Context, input Request) (*Response, error) {
	return c.do(ctx, input, true)
}

func (c *Client) do(ctx context.Context, input Request, readErrorBody bool) (*Response, error) {
	if strings.TrimSpace(input.URL) == "" {
		return nil, fmt.Errorf("url is empty")
	}
	c.mu.RLock()
	cfg := c.config
	c.mu.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	method := input.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, input.URL, bytes.NewReader(input.Body))
	if err != nil {
		return nil, fmt.Errorf("create html request: %w", err)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	for key, value := range cfg.Headers {
		req.Header.Set(key, value)
	}
	for _, headers := range []map[string]string{core.HeadersFrom(ctx), input.Headers} {
		for key, value := range headers {
			if value == "" {
				req.Header.Del(key)
			} else {
				req.Header.Set(key, value)
			}
		}
	}
	client := *c.httpClient
	if input.NoRedirect {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	if session, ok := ctx.Value(sessionKey{}).(*Session); ok && session != nil {
		client.Jar = session.jar
	}
	// This wrapper runs outside governance/proxy transports but inside net/http's
	// cookie and redirect processing. Challenge cookies never reach a live jar.
	client.Transport = challengeTransport{base: client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		var challenge *siteChallengeResponse
		if errors.As(err, &challenge) {
			if challenge.response.Body != nil {
				defer challenge.response.Body.Close()
			}
			return &Response{URL: challenge.url, StatusCode: challenge.response.StatusCode, Header: challenge.response.Header.Clone()}, challenge.err
		}
		return nil, fmt.Errorf("fetch html: %w", err)
	}
	defer resp.Body.Close()
	out := &Response{URL: resp.Request.URL, StatusCode: resp.StatusCode, Header: resp.Header.Clone()}
	var statusErr error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr = &StatusError{Code: resp.StatusCode}
		if !readErrorBody {
			return out, statusErr
		}
	}
	// Avoid max+1 overflowing when a caller configures MaxInt64.
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBodySize))
	if err != nil {
		return out, errors.Join(statusErr, fmt.Errorf("read html response: %w", err))
	}
	var extra [1]byte
	n, err := io.ReadFull(resp.Body, extra[:])
	if n > 0 {
		return out, errors.Join(statusErr, &BodyTooLargeError{MaxBodySize: cfg.MaxBodySize})
	}
	if err != nil && err != io.EOF {
		return out, errors.Join(statusErr, fmt.Errorf("read html response: %w", err))
	}
	out.Body = body
	return out, statusErr
}
