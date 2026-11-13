package app

import (
	"context"
	"net/http"
	"sync"
)

// requestDrain owns HTTP handler admission, including handlers which hijack a
// connection. Closing a connection does not imply its handler has returned.
// It never wraps ResponseWriter, preserving optional interfaces for extensions.
type requestDrain struct {
	mu      sync.Mutex
	closing bool
	pending int
	done    chan struct{}
}

func newRequestDrain() *requestDrain { return &requestDrain{done: make(chan struct{})} }

func (d *requestDrain) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.closing {
			d.mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		d.pending++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.pending--
			if d.closing && d.pending == 0 {
				close(d.done)
			}
			d.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

func (d *requestDrain) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closing {
		d.closing = true
		if d.pending == 0 {
			close(d.done)
		}
	}
}

func (d *requestDrain) wait(ctx context.Context) bool {
	if d == nil {
		return true
	}
	select {
	case <-d.done:
		return true
	default:
	}
	select {
	case <-d.done:
		return true
	case <-ctx.Done():
		return false
	}
}
