package governance

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

type contextKey uint8

const (
	siteKey contextKey = iota
	controllerKey
	transportAdmissionKey
)

func WithSite(ctx context.Context, site string) context.Context {
	return context.WithValue(ctx, siteKey, site)
}
func SiteFromContext(ctx context.Context) string { site, _ := ctx.Value(siteKey).(string); return site }
func WithController(ctx context.Context, c *Controller) context.Context {
	return context.WithValue(ctx, controllerKey, c)
}
func FromContext(ctx context.Context) *Controller {
	c, _ := ctx.Value(controllerKey).(*Controller)
	return c
}

type transport struct {
	base       http.RoundTripper
	controller *Controller
}

func (t *transport) BaseTransport() http.RoundTripper { return t.base }
func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// WithBaseTransport lets an egress client rebuild wrapper chains while retaining
// this wrapper's fallback controller and avoiding nested policy enforcement.
func (t *transport) WithBaseTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, controller: t.controller}
}

// WrapTransport is idempotent. A context-bound controller applies only when the
// caller explicitly identifies the site; unrelated outbound traffic is untouched.
func WrapTransport(base http.RoundTripper) http.RoundTripper { return wrapTransport(base, nil) }
func (c *Controller) WrapTransport(base http.RoundTripper) http.RoundTripper {
	return wrapTransport(base, c)
}
func wrapTransport(base http.RoundTripper, c *Controller) http.RoundTripper {
	// Egress rebuilding may preserve our wrapper below proxy/auth wrappers.
	// Never add a second admission layer just because it is not the outermost.
	for current, depth := base, 0; current != nil && depth < 8; depth++ {
		if existing, ok := current.(*transport); ok && (c == nil || existing.controller == c) {
			return base
		}
		next, ok := current.(interface{ BaseTransport() http.RoundTripper })
		if !ok {
			break
		}
		current = next.BaseTransport()
	}
	if old, ok := base.(*transport); ok {
		if old.controller == c {
			return old
		}
		base = old.base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, controller: c}
}
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Also guard deliberately composed controller wrappers: an exchange can
	// consume only one request permit, even when external wrappers are opaque.
	if req.Context().Value(transportAdmissionKey) != nil {
		return t.base.RoundTrip(req)
	}
	c := FromContext(req.Context())
	if c == nil {
		c = t.controller
	}
	site := SiteFromContext(req.Context())
	if c == nil || site == "" {
		return t.base.RoundTrip(req)
	}
	release, err := c.AcquireRequest(req.Context(), site)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	done := func() { once.Do(func() { release(Feedback{}) }) }
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			done()
			panic(p)
		}
	}()
	resp, err := t.base.RoundTrip(req.WithContext(context.WithValue(req.Context(), transportAdmissionKey, true)))
	f := Feedback{Latency: time.Since(start), Failed: err != nil || resp == nil}
	if resp != nil {
		f.StatusCode = resp.StatusCode
		f.RetryAfter = resp.Header.Get("Retry-After")
	}
	c.Observe(site, f)
	if err != nil || resp == nil {
		done()
		return resp, err
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		done()
		return resp, nil
	}
	stop := context.AfterFunc(req.Context(), done)
	resp.Body = &responseBody{ReadCloser: resp.Body, done: func() { stop(); done() }}
	return resp, nil
}

type responseBody struct {
	io.ReadCloser
	done func()
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.done()
	}
	return n, err
}
func (b *responseBody) Close() error { defer b.done(); return b.ReadCloser.Close() }
