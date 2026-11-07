package app

import (
	"context"
	"time"
)

// UseShutdown registers business cleanup before Run. Hooks run in reverse order
// after HTTP stops and before shared resources close. A timeout/error preserves
// the database and browser pool for workers that may still be settling writes.
// Hooks must honor ctx; they must not log credentials in returned errors.
func (a *App) UseShutdown(fn func(context.Context) error) {
	if fn == nil {
		panic("UseShutdown: nil callback")
	}
	a.shutdownHooks = append(a.shutdownHooks, fn)
}

func (a *App) stopExtensions(timeout time.Duration) bool {
	if len(a.shutdownHooks) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		ok := true
		for i := len(a.shutdownHooks) - 1; i >= 0; i-- {
			func() {
				defer func() {
					if recover() != nil {
						ok = false
					}
				}()
				if a.shutdownHooks[i](ctx) != nil {
					ok = false
				}
			}()
		}
		done <- ok
	}()
	var drained bool
	select {
	case drained = <-done:
	case <-ctx.Done():
	}
	if !drained && a.Logger != nil {
		a.Logger.Sys.Warn("business shutdown incomplete; shared database and browser pool remain open")
	}
	return drained
}
