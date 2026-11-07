package webaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertRenderReleased(t *testing.T, cfg Config, s *Session) {
	t.Helper()
	entries, err := os.ReadDir(cfg.Renderer.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("render left profiles: entries=%d err=%v", len(entries), err)
	}
	if len(s.renderGate) != 0 {
		t.Fatal("render retained session slot")
	}
	browsers.Lock()
	_, held := browsers.limits[s]
	browsers.Unlock()
	if held {
		t.Fatal("render retained global browser slot")
	}
}

func TestRealChromiumEmptyResponseBodies(t *testing.T) {
	for _, kind := range []string{"document", "script", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			var scripts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				if kind == "script" {
					if r.URL.Path == "/empty.js" {
						scripts.Add(1)
						w.Header().Set("Content-Type", "application/javascript")
						w.WriteHeader(http.StatusOK)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, `<html><body><script src="/empty.js"></script><p>after empty script</p></body></html>`)
					return
				}
				if kind == "redirect" && r.URL.Path != "/landing" {
					w.Header().Set("Location", "/landing")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK) // Deliberately no response body.
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, _, err := s.Render(context.Background(), srv.URL+"/start", RenderOptions{ObserveDuration: 50 * time.Millisecond})
			if err != nil || !r.Rendered || !strings.Contains(string(r.Body), "<html>") {
				t.Fatalf("empty %s: err=%v rendered=%v body=%q", kind, err, r.Rendered, r.Body)
			}
			if kind == "redirect" && (r.URL != srv.URL+"/landing" || s.Usage().Pages != 1) {
				t.Fatalf("empty redirect changed navigation: url=%s usage=%+v", r.URL, s.Usage())
			}
			if kind == "script" && (scripts.Load() != 1 || !strings.Contains(string(r.Body), "after empty script")) {
				t.Fatal("empty script was not fulfilled")
			}
			assertRenderReleased(t, cfg, s)
		})
	}
}

func TestRealChromiumRenderBudgetsReleaseResources(t *testing.T) {
	for _, kind := range []string{"requests", "dom-bytes", "total-bytes", "pages", "redirects"} {
		t.Run(kind, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				switch kind {
				case "requests":
					fmt.Fprint(w, `<html><body><script>fetch('/api/one');fetch('/api/two');fetch('/api/three')</script></body></html>`)
				case "dom-bytes", "total-bytes":
					// The wire body fits; only the rendered, multi-byte DOM exceeds
					// its cap. This tests CDP extraction, not HTTP body rejection.
					fmt.Fprint(w, `<html><body><script>document.body.textContent='界'.repeat(700)</script></body></html>`)
				case "pages":
					if r.URL.Path == "/next" {
						fmt.Fprint(w, "unexpected second page")
						return
					}
					fmt.Fprint(w, `<html><script>location.replace('/next')</script></html>`)
				case "redirects":
					http.Redirect(w, r, "/next"+r.URL.Path, http.StatusFound)
				}
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			switch kind {
			case "requests":
				cfg.Budgets.MaxRequests = 3
			case "dom-bytes":
				cfg.Budgets.MaxResponseBytes = 1024
			case "total-bytes":
				cfg.Budgets.MaxTotalBytes = 1500
			case "pages":
				cfg.Budgets.MaxPages = 1
			case "redirects":
				cfg.Budgets.MaxRedirects = 1
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, records, err := s.Render(context.Background(), srv.URL, RenderOptions{ObserveDuration: 300 * time.Millisecond, RecordAPI: true})
			if !errors.Is(err, ErrBudget) || len(r.Body) != 0 || len(records) != 0 {
				t.Fatalf("budget result: err=%v body=%d records=%d usage=%+v hits=%d", err, len(r.Body), len(records), s.Usage(), hits.Load())
			}
			if kind == "requests" && (s.Usage().Requests != 3 || hits.Load() > 3) {
				t.Fatalf("request cap: usage=%+v hits=%d", s.Usage(), hits.Load())
			}
			if kind == "total-bytes" && s.Usage().Bytes != 1500 {
				t.Fatalf("overflow not charged to cap: %+v", s.Usage())
			}
			assertRenderReleased(t, cfg, s)
		})
	}
}

func TestRealChromiumInFlightCancelAndQueuedRender(t *testing.T) {
	for _, name := range []string{"caller-cancel", "session-close", "session-deadline"} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><body>ready</body></html>`)
				once.Do(func() { close(started) })
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			if name == "session-deadline" {
				cfg.Budgets.MaxDuration = 5 * time.Second
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := s.Render(ctx, srv.URL, RenderOptions{ObserveDuration: 20 * time.Second})
				done <- err
			}()
			// Always join the active render before temp directories/server close,
			// including assertion failures in the queued-cancellation path.
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("active renderer did not stop")
					}
				}
			}()
			select {
			case <-started:
			case err := <-done:
				joined = true
				t.Fatalf("render stopped before request: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("render never reached local page")
			}
			// Another session waits on the real global slot; canceling it must
			// not release the first session's browser or leak its own profile.
			queuedCfg := browserConfig(t, srv.URL)
			queued, err := New(queuedCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer queued.Close()
			queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_, _, queuedErr := queued.Render(queuedCtx, srv.URL, RenderOptions{})
			queuedCancel()
			if !errors.Is(queuedErr, ErrCanceled) {
				t.Fatalf("queued cancellation=%v", queuedErr)
			}
			assertRenderReleased(t, queuedCfg, queued)
			browsers.Lock()
			_, active := browsers.limits[s]
			browsers.Unlock()
			if !active {
				t.Fatal("queued cancellation released another render")
			}
			if name == "session-close" {
				_ = s.Close()
			} else if name == "caller-cancel" {
				cancel()
			}
			select {
			case err := <-done:
				joined = true
				if !errors.Is(err, ErrCanceled) {
					t.Fatalf("in-flight cancellation=%v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("render cancellation did not finish")
			}
			assertRenderReleased(t, cfg, s)
			// A fresh render must be able to acquire the released global slot.
			r, _, err := queued.Render(context.Background(), srv.URL, RenderOptions{ObserveDuration: 50 * time.Millisecond})
			if err != nil || !strings.Contains(string(r.Body), "ready") {
				t.Fatalf("slot not reusable: %v", err)
			}
			assertRenderReleased(t, queuedCfg, queued)
		})
	}
}
