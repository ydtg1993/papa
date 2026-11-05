package browser

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// WithIsolatedBrowser borrows one browser process for the lifetime of fn and
// creates an independent Chromium context. Cookies and origin storage survive
// between pages inside fn, but are discarded when fn returns or panics. Cancellation interrupts browser
// operations; fn must cooperate with ctx and finish before cleanup can run.
// Pool default headers/cookies are not inherited; set account headers explicitly
// in PageOptions. Do not retain the Browser or its pages after fn returns.
func (p *Pool) WithIsolatedBrowser(ctx context.Context, fn func(*Browser) error) error {
	if fn == nil {
		return fmt.Errorf("isolated browser: callback required")
	}
	return p.WithBrowser(ctx, func(parent *Browser) error { return parent.withIsolatedContext(ctx, fn) })
}

// WithDirectIsolatedBrowser is the same operation using the direct pool.
func (p *Pool) WithDirectIsolatedBrowser(ctx context.Context, fn func(*Browser) error) error {
	if fn == nil {
		return fmt.Errorf("isolated browser: callback required")
	}
	return p.WithDirectBrowser(ctx, func(parent *Browser) error { return parent.withIsolatedContext(ctx, fn) })
}

func (parent *Browser) withIsolatedContext(ctx context.Context, fn func(*Browser) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	isolated, err := parent.Browser.Context(ctx).Incognito()
	if err != nil {
		return fmt.Errorf("create isolated browser context: %w", err)
	}
	defer func() {
		// Cleanup must not inherit the callback's cancelled deadline.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if closeErr := isolated.Context(cleanup).Close(); closeErr != nil {
			// Do not return a process with unconfirmed credential cleanup to the pool.
			parent.Close()
			err = errors.Join(err, fmt.Errorf("dispose isolated browser context: %w", closeErr))
		}
	}()
	child := &Browser{Browser: isolated, pool: parent.pool, useProxy: parent.useProxy, defaultDevice: parent.defaultDevice, isolatedHeaders: map[string]string{}}
	return fn(child)
}
