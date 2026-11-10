package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/ydtg1993/papa/v4/core"
)

type selectedURLKey struct{}
type poolModeKey struct{}

// WithPool controls pool selection for this request. A specific core proxy URL
// takes precedence. This preserves callers that explicitly opt out of the pool.
func WithPool(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, poolModeKey{}, enabled)
}

// SelectedURL returns the address already bound to this exchange, including a
// deliberate direct connection (empty address, true). A Transport.Proxy function
// must use this value instead of selecting a second proxy.
func SelectedURL(ctx context.Context) (string, bool) {
	address, ok := ctx.Value(selectedURLKey{}).(string)
	return address, ok
}

// RequestProxy is a standard http.Transport.Proxy callback for WrapTransport.
// Egress validation still runs in the underlying transport.
func RequestProxy(req *http.Request) (*url.URL, error) {
	address, selected := SelectedURL(req.Context())
	if !selected {
		address = core.ProxyURLFrom(req.Context())
	}
	if address == "" {
		return nil, nil
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, ErrUnavailable
	}
	return u, nil
}

type healthTransport struct {
	base    http.RoundTripper
	manager *Manager
	fixed   string
	usePool bool
}

// WrapTransport selects and records one proxy per HTTP exchange, including
// redirects. Wrap it outside egress.Client so egress retains its validation.
// The underlying Transport.Proxy callback must consume SelectedURL or RequestProxy.
func WrapTransport(base http.RoundTripper, manager *Manager) http.RoundTripper {
	return &healthTransport{base: base, manager: manager, usePool: true}
}

// WrapFixedTransport records each exchange on the exact fixed proxy configured
// in base. It never rotates or inherits another proxy from request context.
func WrapFixedTransport(base http.RoundTripper, manager *Manager, address string) http.RoundTripper {
	return &healthTransport{base: base, manager: manager, fixed: address}
}

func (t *healthTransport) BaseTransport() http.RoundTripper { return t.base }

// WithBaseTransport preserves selection and feedback when a secured client is
// copied and its underlying egress policy is rebuilt.
func (t *healthTransport) WithBaseTransport(base http.RoundTripper) http.RoundTripper {
	clone := *t
	clone.base = base
	return &clone
}

func (t *healthTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *healthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	address := t.fixed
	if t.usePool {
		address = core.ProxyURLFrom(req.Context())
	}
	var ref *Reference
	var err error
	if address != "" {
		ref, err = t.manager.AcquireURL(req.Context(), address)
	} else if use, ok := req.Context().Value(poolModeKey{}).(bool); !t.usePool || (ok && !use) {
		ref = &Reference{}
	} else {
		ref, err = t.manager.Acquire(req.Context())
	}
	if err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			ref.Release()
		}
	}()
	ctx := context.WithValue(req.Context(), selectedURLKey{}, ref.URL())
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		ref.ReportHTTP(0, err)
		return resp, err
	}
	if resp == nil || resp.Body == nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		ref.ReportHTTP(status, nil)
		return resp, nil
	}
	// Keep retired proxy identity alive until the response body is finished.
	resp.Body = &feedbackBody{ReadCloser: resp.Body, ref: ref, status: resp.StatusCode}
	handedOff = true
	return resp, nil
}

type feedbackBody struct {
	io.ReadCloser
	ref    *Reference
	status int
	once   sync.Once
}

func (b *feedbackBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		feedbackErr := err
		if feedbackErr == io.EOF {
			feedbackErr = nil
		}
		b.finish(feedbackErr)
	}
	return n, err
}

func (b *feedbackBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(err)
	return err
}

func (b *feedbackBody) finish(err error) {
	b.once.Do(func() { b.ref.ReportHTTP(b.status, err) })
}
