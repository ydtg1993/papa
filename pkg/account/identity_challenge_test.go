package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestSiteChallengeAccountCustomClassifierNeverInvalidates(t *testing.T) {
	for _, status := range []int{200, 401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("cf-mitigated", "challenge")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{server.URL}}})
			m := NewManager(client)
			key := Key{Site: "fixture", ID: "alice"}
			var logins, classifies atomic.Int32
			if err := m.Register(key, Config{Origins: []string{server.URL}, CheckInterval: time.Hour, Login: func(_ context.Context, c *Credentials) error {
				logins.Add(1)
				c.Headers["Authorization"] = "fixture-token"
				return nil
			}, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }, Classify: func(*htmlfetch.Response, error) Outcome { classifies.Add(1); return Expired }}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := m.Do(ctx, key, htmlfetch.Request{URL: server.URL}); !errors.Is(err, core.ErrSiteChallenge) {
				t.Fatal(err)
			}
			before := m.Status()[0]
			if before.Version != 1 || !before.HTTPReady || logins.Load() != 1 || classifies.Load() != 0 {
				t.Fatal(before, logins.Load(), classifies.Load())
			}
			pools, err := NewPools(ctx, m, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer pools.Close()
			pk := PoolKey{Site: key.Site, Name: "members"}
			if _, err = pools.Upsert(ctx, PoolConfig{Key: pk, Members: []string{key.ID}}, 0); err != nil {
				t.Fatal(err)
			}
			attempt, err := pools.Acquire(ctx, pk, HTTP)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.DoPool(WithAttempt(ctx, attempt), pk, htmlfetch.Request{URL: server.URL}); !errors.Is(err, core.ErrSiteChallenge) {
				t.Fatal(err)
			}
			if err = pools.ValidateAttempt(attempt); err != nil {
				t.Fatal("site challenge changed account/selection version", err)
			}
			if m.Status()[0].Version != before.Version || classifies.Load() != 0 || logins.Load() != 1 {
				t.Fatal("site challenge mutated account")
			}
		})
	}
}

func TestSiteChallengeAccountCannotDeleteExistingSessionCookie(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/challenge" {
					w.Header().Set("cf-mitigated", "challenge")
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1})
					w.WriteHeader(status)
					return
				}
				cookie, err := r.Cookie("session")
				if err != nil || cookie.Value != "original-session" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte("original-session"))
			}))
			defer server.Close()
			client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{server.URL}}})
			manager := NewManager(client)
			key := Key{Site: "fixture", ID: "alice"}
			origin, _ := url.Parse(server.URL)
			var logins atomic.Int32
			if err := manager.Register(key, Config{Origins: []string{server.URL}, CheckInterval: time.Hour, Login: func(_ context.Context, credentials *Credentials) error {
				logins.Add(1)
				credentials.Session.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "original-session", Path: "/"}})
				return nil
			}, Check: func(context.Context, *Credentials) (bool, error) { return true, nil }}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := manager.Do(ctx, key, htmlfetch.Request{URL: server.URL}); err != nil {
				t.Fatal(err)
			}
			before := manager.Status()[0].Version
			if _, err := manager.Do(ctx, key, htmlfetch.Request{URL: server.URL + "/challenge"}); !errors.Is(err, core.ErrSiteChallenge) {
				t.Fatal(err)
			}
			response, err := manager.Do(ctx, key, htmlfetch.Request{URL: server.URL})
			if err != nil || string(response.Body) != "original-session" || logins.Load() != 1 || manager.Status()[0].Version != before {
				t.Fatal("direct challenge deleted the existing session cookie", err)
			}
			pools, err := NewPools(ctx, manager, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer pools.Close()
			pool := PoolKey{Site: key.Site, Name: "members"}
			if _, err = pools.Upsert(ctx, PoolConfig{Key: pool, Members: []string{key.ID}}, 0); err != nil {
				t.Fatal(err)
			}
			attempt, err := pools.Acquire(ctx, pool, HTTP)
			if err != nil {
				t.Fatal(err)
			}
			poolCtx := WithAttempt(ctx, attempt)
			if _, err = manager.DoPool(poolCtx, pool, htmlfetch.Request{URL: server.URL + "/challenge"}); !errors.Is(err, core.ErrSiteChallenge) {
				t.Fatal(err)
			}
			response, err = manager.DoPool(poolCtx, pool, htmlfetch.Request{URL: server.URL})
			if err != nil || string(response.Body) != "original-session" || logins.Load() != 1 || pools.ValidateAttempt(attempt) != nil {
				t.Fatal("pooled challenge deleted the existing session cookie", err)
			}
		})
	}
}
