package webaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

func browserConfig(t *testing.T, raw string) Config {
	t.Helper()
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("real Chromium unavailable; controlled rendering was not verified")
	}
	cfg := localConfig(raw)
	cfg.Renderer = RendererConfig{Enabled: true, ExecutablePath: bin, TempDir: t.TempDir()}
	cfg.Budgets.MaxDuration = 30 * time.Second
	return cfg
}

func TestRealChromiumRenderAndRecordGuardedGET(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("uncontrolled headers: %v", r.Header)
		}
		hits.Add(1)
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/landing/index", http.StatusFound)
		case "/robots.txt":
			http.NotFound(w, r)
		case "/landing/index":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><div id="out">loading</div><iframe src="frame"></iframe><script src="script"></script></body></html>`)
		case "/landing/frame":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<p>frame</p>`)
		case "/landing/script":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, `document.cookie="forbidden=secret"; fetch('api').then(r=>r.json()).then(x=>document.querySelector('#out').textContent=x.value); Object.defineProperty(document.documentElement,'outerHTML',{get:()=> 'forged evidence'}); window.TextEncoder = class { encode() { return {byteLength:0} } }`)
		case "/landing/api":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"value":"rendered marker"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, records, err := s.Render(context.Background(), srv.URL+"/start", RenderOptions{ObserveDuration: 500 * time.Millisecond, RecordAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Rendered || !strings.Contains(string(r.Body), ">rendered marker</div>") || len(records) != 1 || !strings.Contains(string(records[0].Body), "rendered marker") {
		t.Fatalf("response=%+v records=%+v", r, records)
	}
	if r.StatusCode != 0 || records[0].StatusCode != http.StatusOK || records[0].Rendered {
		t.Fatalf("DOM/API status semantics mixed: DOM=%+v API=%+v", r, records[0])
	}
	if r.URL != srv.URL+"/landing/index" || records[0].URL != srv.URL+"/landing/api" {
		t.Fatalf("redirect base URL lost: page=%s api=%s", r.URL, records[0].URL)
	}
	if int(hits.Load()) != s.Usage().Requests {
		t.Fatalf("server=%d usage=%+v", hits.Load(), s.Usage())
	}
	if s.Usage().Pages != 1 {
		t.Fatalf("redirect counted as page: %+v", s.Usage())
	}
	entries, err := os.ReadDir(cfg.Renderer.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("profile not cleaned: %v %v", entries, err)
	}
}

func TestRealChromiumNativeEgressDenyProxy(t *testing.T) {
	for _, kind := range []string{"popup", "worker", "websocket", "iframe", "resource", "redirect", "post"} {
		t.Run(kind, func(t *testing.T) {
			var hits atomic.Int32
			denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/javascript")
				fmt.Fprint(w, `postMessage('escaped')`)
			}))
			defer denied.Close()
			var serverURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				var script string
				switch kind {
				case "popup":
					script = fmt.Sprintf(`window.open(%q)`, denied.URL+"/popup")
				case "worker":
					script = fmt.Sprintf(`new Worker(URL.createObjectURL(new Blob([%q],{type:'application/javascript'})))`, `fetch('`+denied.URL+`/worker')`)
				case "websocket":
					script = fmt.Sprintf(`new WebSocket(%q)`, strings.Replace(denied.URL, "http:", "ws:", 1)+"/socket")
				case "iframe":
					script = fmt.Sprintf(`document.body.innerHTML += '<iframe src="%s/iframe"></iframe>'`, denied.URL)
				case "resource":
					script = fmt.Sprintf(`new Image().src=%q`, denied.URL+"/image")
				case "redirect":
					if r.URL.Path == "/redirect" {
						http.Redirect(w, r, denied.URL+"/redirected", 302)
						return
					}
					script = fmt.Sprintf(`fetch(%q)`, serverURL+"/redirect")
				case "post":
					script = fmt.Sprintf(`fetch(%q,{method:'POST',body:'secret'})`, denied.URL+"/post")
				}
				fmt.Fprintf(w, `<html><body>attempted<script>%s;document.body.dataset.attempted='yes'</script></body></html>`, script)
			}))
			defer srv.Close()
			serverURL = srv.URL
			cfg := browserConfig(t, srv.URL)
			s, _ := New(cfg)
			defer s.Close()
			r, _, err := s.Render(context.Background(), srv.URL, RenderOptions{ObserveDuration: 700 * time.Millisecond})
			if err != nil && !errors.Is(err, ErrPolicy) {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && !strings.Contains(string(r.Body), `data-attempted="yes"`) {
				t.Fatalf("attack script did not run: %s", r.Body)
			}
			if hits.Load() != 0 {
				t.Fatalf("native egress escaped for %s: %d hits", kind, hits.Load())
			}
		})
	}
}

func TestRealChromiumCancelReleasesProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html>ready</html>`)
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	s, _ := New(cfg)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := s.Render(ctx, srv.URL, RenderOptions{ObserveDuration: 10 * time.Second})
	if !errors.Is(err, ErrCanceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cfg.Renderer.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("profile not cleaned after cancel: %v %v", entries, err)
	}
}
