package htmlfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestSessionGuardHTTPExpiryAndConcurrentLogin(t *testing.T) {
	var revision, logins atomic.Int64
	revision.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: fmt.Sprint(revision.Load()), Path: "/"})
			return
		}
		c, err := r.Cookie("sid")
		if err != nil || c.Value != fmt.Sprint(revision.Load()) {
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	client := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	g, err := NewSessionGuard(SessionGuardConfig{
		Check: func(ctx context.Context, s *Session) (bool, error) {
			r, err := client.Do(WithSession(ctx, s), Request{URL: srv.URL})
			if r != nil && r.StatusCode == 401 {
				return false, nil
			}
			return err == nil, err
		},
		Login: func(ctx context.Context, s *Session) error {
			_, err := client.Do(WithSession(ctx, s), Request{URL: srv.URL + "/login", Method: "POST"})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := g.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	revision.Store(2)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := g.Ensure(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if s == first {
				t.Error("expired session returned")
			}
		}()
	}
	wg.Wait()
	if logins.Load() != 2 {
		t.Fatalf("login calls=%d want 2", logins.Load())
	}
}

func TestSessionGuardRejectsCandidateAndCoolsDown(t *testing.T) {
	var calls atomic.Int64
	g, _ := NewSessionGuard(SessionGuardConfig{Check: func(context.Context, *Session) (bool, error) { return false, nil }, Login: func(context.Context, *Session) error { calls.Add(1); return nil }, FailureCooldown: time.Hour})
	for i := 0; i < 2; i++ {
		s, err := g.Ensure(context.Background())
		if s != nil || !errors.Is(err, ErrSessionRejected) {
			t.Fatalf("rejected: %v %v", s, err)
		}
	}
	if calls.Load() != 1 || g.current != nil {
		t.Fatal("failed candidate published or cooldown ignored")
	}
	// Expire the deadline directly rather than sleeping in the test.
	g.mu.Lock()
	g.retryAt = time.Now().Add(-time.Second)
	g.mu.Unlock()
	_, _ = g.Ensure(context.Background())
	if calls.Load() != 2 {
		t.Fatal("cooldown never expired")
	}
}

func TestSessionGuardCheckErrorDoesNotLogin(t *testing.T) {
	problem := errors.New("probe unavailable")
	var checks, logins int
	g, _ := NewSessionGuard(SessionGuardConfig{Check: func(context.Context, *Session) (bool, error) {
		checks++
		if checks > 1 {
			return false, problem
		}
		return true, nil
	}, Login: func(context.Context, *Session) error { logins++; return nil }})
	old, err := g.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.Ensure(context.Background())
	if s != nil || !errors.Is(err, problem) || logins != 1 || g.current != old {
		t.Fatalf("probe error: %v %v login=%d", s, err, logins)
	}
}

func TestSessionGuardWaiterCancellationAndLeaderCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	g, _ := NewSessionGuard(SessionGuardConfig{Check: func(context.Context, *Session) (bool, error) { return true, nil }, Login: func(ctx context.Context, _ *Session) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	done := make(chan error, 1)
	go func() { _, err := g.Ensure(context.Background()); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Ensure(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait cancel: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := g.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionGuardTimeoutAndPanicRelease(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		g, _ := NewSessionGuard(SessionGuardConfig{Timeout: 10 * time.Millisecond, Check: func(context.Context, *Session) (bool, error) { return true, nil }, Login: func(ctx context.Context, _ *Session) error { <-ctx.Done(); return ctx.Err() }})
		if _, err := g.Ensure(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if g.flight != nil || g.failure != nil || g.current != nil {
			t.Fatal("timeout retained flight or candidate")
		}
	})
	t.Run("panic", func(t *testing.T) {
		g, _ := NewSessionGuard(SessionGuardConfig{Check: func(context.Context, *Session) (bool, error) { return true, nil }, Login: func(context.Context, *Session) error { panic("login panic") }})
		func() {
			defer func() {
				if recover() != "login panic" {
					t.Error("panic lost")
				}
			}()
			_, _ = g.Ensure(context.Background())
		}()
		if g.flight != nil {
			t.Fatal("panic left locked flight")
		}
		if _, err := g.Ensure(context.Background()); !errors.Is(err, ErrSessionCallbackPanic) {
			t.Fatalf("panic cooldown: %v", err)
		}
	})
}
