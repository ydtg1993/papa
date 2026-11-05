package htmlfetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrSessionRejected = errors.New("session verification failed after login")
var ErrSessionCallbackPanic = errors.New("session callback panicked")

// SessionGuardConfig is specific to one account. Check must be read-only and
// return false,nil only for a known expired/unauthenticated session, not transport
// errors. Login must modify only its candidate Session. Neither may reenter Ensure.
type SessionGuardConfig struct {
	Check           func(context.Context, *Session) (bool, error)
	Login           func(context.Context, *Session) error
	Timeout         time.Duration
	FailureCooldown time.Duration
}

type sessionFlight struct {
	done    chan struct{}
	session *Session
	err     error
}

// SessionGuard publishes only verified sessions. It coordinates one process;
// it does not replay business requests or persist credentials. Do not copy it.
type SessionGuard struct {
	mu      sync.Mutex
	config  SessionGuardConfig
	current *Session
	flight  *sessionFlight
	failure error
	retryAt time.Time
}

func NewSessionGuard(cfg SessionGuardConfig) (*SessionGuard, error) {
	if cfg.Check == nil || cfg.Login == nil {
		return nil, fmt.Errorf("session guard requires Check and Login")
	}
	if cfg.Timeout < 0 || cfg.FailureCooldown < 0 {
		return nil, fmt.Errorf("session guard durations cannot be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.FailureCooldown == 0 {
		cfg.FailureCooldown = 5 * time.Second
	}
	return &SessionGuard{config: cfg}, nil
}

// Ensure checks the current session or logs in and verifies a new candidate.
// Concurrent callers share a round; only the leading caller executes callbacks.
// Its cancellation ends the shared round; waiters can cancel their own wait.
// Timeout is cooperative: callbacks must honor ctx. Ordinary failures are cached
// during FailureCooldown; cancellation/deadline failures are not cached.
func (g *SessionGuard) Ensure(ctx context.Context) (session *Session, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if f := g.flight; f != nil {
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return f.session, f.err
		}
	}
	if g.failure != nil && time.Now().Before(g.retryAt) {
		err := g.failure
		g.mu.Unlock()
		return nil, err
	}
	f := &sessionFlight{done: make(chan struct{})}
	g.flight = f
	current := g.current
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()
	defer func() {
		panicValue := recover()
		if panicValue != nil {
			session = nil
			err = ErrSessionCallbackPanic
		}
		if err == nil && ctx.Err() != nil {
			session = nil
			err = ctx.Err()
		}
		g.mu.Lock()
		if err == nil {
			g.current = session
			g.failure = nil
			g.retryAt = time.Time{}
		} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			g.failure = err
			g.retryAt = time.Now().Add(g.config.FailureCooldown)
		}
		f.session, f.err = session, err
		g.flight = nil
		close(f.done)
		g.mu.Unlock()
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	if current != nil {
		valid, checkErr := g.config.Check(ctx, current)
		if checkErr != nil {
			return nil, fmt.Errorf("check session: %w", checkErr)
		}
		if valid {
			return current, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidate := NewSession()
	if err := g.config.Login(ctx, candidate); err != nil {
		return nil, fmt.Errorf("login session: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	valid, err := g.config.Check(ctx, candidate)
	if err != nil {
		return nil, fmt.Errorf("verify session: %w", err)
	}
	if !valid {
		return nil, ErrSessionRejected
	}
	return candidate, nil
}
