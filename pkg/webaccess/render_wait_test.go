package webaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func TestRenderWaitInvalidBeforeWork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfg := localConfig(srv.URL)
	cfg.Renderer.Enabled = true
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, w := range []RenderWait{{}, {"#ok", "exists", 0}, {"#ok", "exists", 31 * time.Second}, {"#ok", "hidden", time.Second}, {"a:has(b)", "exists", time.Second}, {"[", "exists", time.Second}} {
		if _, _, err := s.Render(context.Background(), srv.URL, RenderOptions{Wait: &w}); !errors.Is(err, ErrWaitInvalid) {
			t.Fatalf("wait=%+v err=%v", w, err)
		}
	}
	w := RenderWait{"#ok", "exists", time.Second}
	if _, _, err := s.Render(context.Background(), srv.URL, RenderOptions{Wait: &w, ObserveDuration: time.Millisecond}); !errors.Is(err, ErrWaitInvalid) {
		t.Fatal(err)
	}
	if hits.Load() != 0 || s.Usage().Requests != 0 || s.Usage().Pages != 0 {
		t.Fatalf("invalid wait performed work: hits=%d usage=%+v", hits.Load(), s.Usage())
	}
}

func TestRealChromiumRenderWait(t *testing.T) {
	for _, tc := range []struct {
		name, html, selector, state, want string
		fail                              bool
	}{
		{"delayed-exists", `<div id="out">loading</div><script>setTimeout(()=>{document.querySelector('#out').innerHTML='<b id="ready">ready-marker</b>'},800)</script>`, "#ready", "exists", `>ready-marker</b>`, false},
		{"delayed-visible", `<b id="ready" style="display:none">hidden</b><script>setTimeout(()=>{let e=document.querySelector('#ready');e.style.display='block';e.textContent='visible-marker'},800)</script>`, "#ready", "visible", `>visible-marker</b>`, false},
		{"hidden-exists", `<b id="ready" style="display:none">hidden-marker</b>`, "#ready", "exists", `>hidden-marker</b>`, false},
		{"hidden-timeout", `<b id="ready" style="visibility:hidden">hidden</b>`, "#ready", "visible", "", true},
		{"first-match", `<b class="ready" style="display:none">first</b><b class="ready">second</b>`, ".ready", "visible", "", true},
		{"isolated-world", `<script>Document.prototype.querySelector=()=>document.body;window.getComputedStyle=()=>({display:'block',visibility:'visible'})</script>`, "#missing", "visible", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "<html><body>"+tc.html+"</body></html>")
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			timeout := 3 * time.Second
			if tc.fail {
				timeout = 250 * time.Millisecond
			}
			out, records, err := s.Render(context.Background(), srv.URL, RenderOptions{Wait: &RenderWait{tc.selector, tc.state, timeout}, RecordAPI: true})
			if tc.fail {
				if !errors.Is(err, ErrWaitTimeout) || len(out.Body) != 0 || len(records) != 0 {
					t.Fatalf("failed wait produced evidence: err=%v body=%d records=%d", err, len(out.Body), len(records))
				}
			} else if err != nil || !out.Rendered || !strings.Contains(string(out.Body), tc.want) {
				t.Fatalf("ready DOM missing: err=%v body=%s", err, out.Body)
			}
			assertRenderReleased(t, cfg, s)
		})
	}
}

func TestRealChromiumRenderWaitRetainsBoundaries(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline", "session-close", "session-deadline", "policy", "requests", "transport"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			signal := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/signal" {
					if kind == "transport" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					select {
					case signal <- struct{}{}:
					default:
					}
					fmt.Fprint(w, "ok")
					return
				}
				w.Header().Set("Content-Type", "text/html")
				target := "/signal"
				if kind == "policy" {
					target = "http://127.0.0.1:1/denied"
				}
				if kind == "requests" {
					fmt.Fprint(w, `<html><body><script>setTimeout(async()=>{for(let i=0;i<10;i++)await fetch('/signal')},250)</script></body></html>`)
					return
				}
				fmt.Fprintf(w, `<html><body><script>setTimeout(()=>fetch(%q),250)</script></body></html>`, target)
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			if kind == "requests" {
				cfg.Budgets.MaxRequests = 5
			}
			if kind == "session-deadline" {
				cfg.Budgets.MaxDuration = 2 * time.Second
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Cancel after JS is running, rather than during Chromium startup.
			done := make(chan struct{})
			defer close(done)
			if kind == "cancel" || kind == "session-close" {
				go func() {
					select {
					case <-signal:
						if kind == "session-close" {
							_ = s.Close()
						} else {
							cancel()
						}
					case <-done:
					}
				}()
			}
			if kind == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
			}
			started := time.Now()
			out, records, err := s.Render(ctx, srv.URL, RenderOptions{Wait: &RenderWait{"#never", "exists", 10 * time.Second}})
			want := ErrCanceled
			if kind == "policy" {
				want = ErrPolicy
			}
			if kind == "requests" {
				want = ErrBudget
			}
			if kind == "transport" {
				want = ErrTransport
			}
			if !errors.Is(err, want) || len(out.Body) != 0 || len(records) != 0 {
				t.Fatalf("kind=%s err=%v want=%v body=%d", kind, err, want, len(out.Body))
			}
			if kind == "requests" {
				select {
				case <-signal:
				default:
					t.Fatal("request budget failed before asynchronous waiting work")
				}
				if s.Usage().Requests != 5 {
					t.Fatalf("wait requests escaped shared cap: %+v", s.Usage())
				}
			}
			if time.Since(started) > 8*time.Second {
				t.Fatal("wait extended outer boundary")
			}
			assertRenderReleased(t, cfg, s)
		})
	}
}

func TestRealChromiumRenderWaitQueuedCancellation(t *testing.T) {
	signal := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/signal" {
			select {
			case signal <- struct{}{}:
			default:
			}
			fmt.Fprint(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><b id="ready">ready</b><script>setTimeout(()=>fetch('/signal'),250)</script></body></html>`)
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Render(ctx, srv.URL, RenderOptions{Wait: &RenderWait{"#missing", "exists", 10 * time.Second}})
		done <- err
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("active render did not exit")
			}
		}
	}()
	select {
	case <-signal:
	case err := <-done:
		joined = true
		t.Fatalf("render stopped before waiting: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("page script did not start")
	}
	otherCfg := browserConfig(t, srv.URL)
	other, err := New(otherCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _, err = other.Render(queuedCtx, srv.URL, RenderOptions{Wait: &RenderWait{"#ready", "exists", time.Second}})
	queuedCancel()
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("queued wait ignored context: %v", err)
	}
	assertRenderReleased(t, otherCfg, other)
	browsers.Lock()
	_, held := browsers.limits[s]
	browsers.Unlock()
	if !held {
		t.Fatal("queued cancellation released active wait")
	}
	cancel()
	select {
	case err = <-done:
		joined = true
		if !errors.Is(err, ErrCanceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not cancel")
	}
	assertRenderReleased(t, cfg, s)
	out, _, err := other.Render(context.Background(), srv.URL, RenderOptions{Wait: &RenderWait{"#ready", "visible", time.Second}})
	if err != nil || !out.Rendered {
		t.Fatalf("slot reuse failed: %v", err)
	}
	assertRenderReleased(t, otherCfg, other)
}

func TestRealChromiumRenderWaitClosedPage(t *testing.T) {
	// Exercise target-loss classification using only about:blank, the production
	// sandbox/proxy launcher, and a host-initiated close (no interaction tool).
	cfg := browserConfig(t, "http://127.0.0.1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	proxy, closeProxy, err := denyProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy()
	launch := guardedLauncher(ctx, cfg.Renderer.ExecutablePath, t.TempDir(), proxy)
	defer launch.Cleanup()
	defer launch.Kill()
	control, err := launch.Launch()
	if err != nil {
		t.Fatal("controlled Chromium launch failed")
	}
	browser := rod.New().Context(ctx).ControlURL(control)
	if err = browser.Connect(); err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := (proto.PageGetFrameTree{}).Call(page)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- waitRenderDOM(ctx, page, frames.FrameTree.Frame.ID, RenderWait{"#never", "exists", 10 * time.Second})
	}()
	if err = page.Close(); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, ErrRendererUnavailable) {
			t.Fatalf("closed page classified as %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("closed page retained wait until selector timeout")
	}
}

func TestRealChromiumRenderSnapshotStopsInflightResource(t *testing.T) {
	for _, withWait := range []bool{false, true} {
		name := "observe"
		if withWait {
			name = "wait"
		}
		t.Run(name, func(t *testing.T) {
			started, canceled := make(chan struct{}, 1), make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/slow" {
					select {
					case started <- struct{}{}:
					default:
					}
					<-r.Context().Done()
					select {
					case canceled <- struct{}{}:
					default:
					}
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><body><script>fetch('/slow');setTimeout(()=>document.body.insertAdjacentHTML('beforeend','<b id="ready">ready</b>'),800)</script></body></html>`)
			}))
			defer srv.Close()
			cfg := browserConfig(t, srv.URL)
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			opts := RenderOptions{ObserveDuration: time.Second, RecordAPI: true}
			if withWait {
				opts.ObserveDuration = 0
				opts.Wait = &RenderWait{"#ready", "visible", 3 * time.Second}
			}
			out, records, err := s.Render(context.Background(), srv.URL, opts)
			if err != nil || !strings.Contains(string(out.Body), `>ready</b>`) || len(records) != 0 {
				t.Fatalf("snapshot shutdown became transport failure: err=%v body=%d records=%d", err, len(out.Body), len(records))
			}
			select {
			case <-started:
			default:
				t.Fatal("slow request was never in flight")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("snapshot left request running")
			}
			before := s.Usage()
			if before.Requests < 3 {
				t.Fatalf("inflight request was not charged: %+v", before)
			}
			assertRenderReleased(t, cfg, s)
			if s.Usage() != before {
				t.Fatal("usage changed after Render returned")
			}
		})
	}
}
