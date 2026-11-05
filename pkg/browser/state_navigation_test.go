package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func navigationState(rawURL, account string) *PageState {
	u, _ := url.Parse(rawURL)
	return &PageState{
		Version: 1, Origin: rawURL,
		Cookies:      []*proto.NetworkCookie{{Name: "sid", Value: account, Domain: u.Hostname(), Path: "/", HTTPOnly: true, Session: true}},
		LocalStorage: map[string]string{"token": account, "__proto__": "ordinary-key"},
	}
}

func TestWithStatePageSurvivesSiteStorageClearAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			seen := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/seen" {
					seen <- r.URL.Query().Get("token")
					return
				}
				w.Header().Set("Content-Type", "text/html")
				if r.URL.Path == "/start" && redirect {
					_, _ = io.WriteString(w, `<script>debugger;sessionStorage.clear();localStorage.setItem('token','rotated');location.replace('/end')</script>`)
					return
				}
				_, _ = io.WriteString(w, `<script>debugger;window.firstToken=localStorage.getItem('token');sessionStorage.clear();document.write('<img src="/seen?token='+encodeURIComponent(firstToken)+'">')</script>`)
			}))
			defer srv.Close()
			pool := newLivePool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			called := false
			want := "saved"
			if redirect {
				want = "rotated"
			}
			err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
				return b.WithStatePage(ctx, srv.URL+"/start", navigationState(srv.URL, "saved"), PageOptions{WaitJS: "window.firstToken !== undefined"}, func(page *rod.Page) error {
					called = true
					value, err := page.Eval(`() => { debugger; return firstToken; }`)
					if err == nil && value.Value.Str() != want {
						return fmt.Errorf("first script token=%q, want %q", value.Value.Str(), want)
					}
					return err
				})
			})
			select {
			case token := <-seen:
				if token != want {
					t.Errorf("site observed stale token=%q, want %q", token, want)
				}
			case <-ctx.Done():
				t.Error("site did not report first-script token", ctx.Err())
			}
			if err != nil || !called {
				t.Fatalf("site sessionStorage.clear rejected restored state: callback=%v err=%v", called, err)
			}
		})
	}
}

func TestWithStatePageFirstRequestAndScript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/frame" {
			_, _ = io.WriteString(w, `<script>parent.frameToken=localStorage.getItem('token')</script>`)
			return
		}
		cookie, _ := json.Marshal(r.Header.Get("Cookie"))
		_, _ = fmt.Fprintf(w, `<script>
window.requestCookie=%s;
window.firstToken=localStorage.getItem('token');
window.firstCookie=document.cookie;
if(location.pathname==='/first') localStorage.setItem('token','rotated');
</script><iframe src="/frame"></iframe><div id="ready"></div>`, cookie)
	}))
	defer srv.Close()
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		return b.WithStatePage(ctx, srv.URL+"/first", navigationState(srv.URL, "saved"), PageOptions{
			Cookies:      []*proto.NetworkCookieParam{{Name: "sid", Value: "opts", URL: srv.URL}},
			WaitSelector: "#ready", WaitJS: "window.frameToken === 'rotated'",
			Interact: func(_ context.Context, page *rod.Page) error {
				got, err := page.Eval(`() => [requestCookie,firstToken,firstCookie,frameToken,localStorage.getItem('__proto__'),sessionStorage.length].join('|')`)
				if err != nil {
					return err
				}
				if got.Value.Str() != "sid=saved|saved||rotated|ordinary-key|0" {
					return fmt.Errorf("initial request/script state = %q", got.Value.Str())
				}
				if err := page.Navigate(srv.URL + "/next"); err != nil {
					return err
				}
				return page.WaitLoad()
			},
		}, func(page *rod.Page) error {
			got, err := page.Eval(`() => firstToken`)
			if err != nil {
				return err
			}
			if got.Value.Str() != "rotated" {
				return fmt.Errorf("initializer repeated after Interact navigation: %q", got.Value.Str())
			}
			cookies, err := page.Cookies(nil)
			if err == nil && (len(cookies) != 1 || !cookies[0].HTTPOnly || cookies[0].Value != "saved") {
				return fmt.Errorf("HttpOnly cookie attributes lost")
			}
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithStatePageIsolatesAccountsAndAllowsNilState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		cookie, _ := json.Marshal(r.Header.Get("Cookie"))
		_, _ = fmt.Fprintf(w, `<script>window.initial=(localStorage.getItem('token')||'')+'|'+%s;</script>`, cookie)
	}))
	defer srv.Close()
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, account := range []string{"alice", "bob", ""} {
		t.Run(account, func(t *testing.T) {
			var state *PageState
			want := "|"
			if account != "" {
				state = navigationState(srv.URL, account)
				want = account + "|sid=" + account
			}
			if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
				return b.WithStatePage(ctx, srv.URL, state, PageOptions{}, func(page *rod.Page) error {
					got, err := page.Eval(`() => initial`)
					if err == nil && got.Value.Str() != want {
						return fmt.Errorf("account state = %q, want %q", got.Value.Str(), want)
					}
					return err
				})
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWithStatePageValidationBeforeNavigation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := (&Browser{}).WithStatePage(ctx, "https://example.com", nil, PageOptions{}, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	pool := newLivePool(t)
	called := false
	fn := func(*rod.Page) error { called = true; return nil }
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		if err := b.WithStatePage(ctx, "https://example.com", nil, PageOptions{}, fn); err == nil {
			return fmt.Errorf("default context accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		for _, tc := range []struct {
			url   string
			state *PageState
		}{
			{"data:text/html,hello", nil},
			{"https://user:password@example.com", nil},
			{"https://@example.com", nil},
			{"https://example.com", &PageState{Version: 2, Origin: "https://example.com"}},
			{"https://example.org", &PageState{Version: 1, Origin: "https://example.com"}},
			{"https://example.com", &PageState{Version: 1, Origin: "https://example.com", Cookies: []*proto.NetworkCookie{{Name: "sid", Domain: "evil.com", Path: "/"}}}},
		} {
			if err := b.WithStatePage(ctx, tc.url, tc.state, PageOptions{}, fn); err == nil {
				return fmt.Errorf("invalid URL/state accepted")
			}
		}
		return assertStatePagesClosed(b)
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("invalid input reached callback")
	}
}

func TestWithStatePageNilStateOriginGuard(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>external origin</html>")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL, http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "<html>account origin</html>")
	}))
	defer srv.Close()
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, mode := range []string{"redirect", "interact"} {
		t.Run(mode, func(t *testing.T) {
			interacted, called := false, false
			err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
				err := b.WithStatePage(ctx, srv.URL+"/"+mode, nil, PageOptions{
					Interact: func(_ context.Context, page *rod.Page) error {
						interacted = true
						if err := page.Navigate(other.URL); err != nil {
							return err
						}
						return page.WaitLoad()
					},
				}, func(*rod.Page) error { called = true; return nil })
				return errors.Join(err, assertStatePagesClosed(b))
			})
			if err == nil || !strings.Contains(err.Error(), "origin mismatch") || called || interacted != (mode == "interact") {
				t.Fatalf("nil state origin guard: interact=%v callback=%v err=%v", interacted, called, err)
			}
		})
	}
}

func TestWithStatePageNormalizesBrowserOrigin(t *testing.T) {
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		page, err := b.newPageBaseContext(ctx, PageOptions{})
		if err != nil {
			return err
		}
		defer page.Close()
		for _, tc := range []struct{ url, want string }{
			{"HTTPS://ExAmPlE.COM:443/path?q=1#part", "https://example.com"},
			{"http://example.com:80/?#", "http://example.com"},
			{"https://example.com:8443/", "https://example.com:8443"},
			{"http://[2001:0db8::1]:80/", "http://[2001:db8::1]"},
			{"https://bücher.example/", "https://xn--bcher-kva.example"},
		} {
			got, err := stateNavigationOrigin(page, tc.url)
			if err != nil || got != tc.want {
				return fmt.Errorf("normalize %q: got %q, want %q, err=%v", tc.url, got, tc.want, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<script>window.firstToken=localStorage.getItem('token')</script>`)
	}))
	defer srv.Close()
	canonical := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	originalOrigin := strings.Replace(canonical, "localhost", "LOCALHOST", 1)
	state := navigationState(originalOrigin, "saved")
	if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		return b.WithStatePage(ctx, strings.Replace(canonical, "localhost", "LOCALHOST", 1)+"/?q=1#part", state, PageOptions{}, func(page *rod.Page) error {
			got, err := page.Eval(`() => firstToken`)
			if err == nil && got.Value.Str() != "saved" {
				return fmt.Errorf("normalized target did not receive state")
			}
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	if state.Origin != originalOrigin {
		t.Fatal("caller state was changed")
	}
}

func TestWithStatePageRedirects(t *testing.T) {
	seen := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/seen" {
			seen <- r.URL.Query().Get("token")
			return
		}
		_, _ = io.WriteString(w, `<script>document.write('<img src="/seen?token='+encodeURIComponent(localStorage.getItem('token')||'empty')+'">')</script>`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/end", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/script":
			_, _ = io.WriteString(w, `<script>localStorage.setItem('token','rotated');location.replace('/end')</script>`)
		default:
			_, _ = io.WriteString(w, `<script>window.firstToken=localStorage.getItem('token')</script>`)
		}
	}))
	defer srv.Close()
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, path := range []string{"/same", "/script", "/cross"} {
		t.Run(path, func(t *testing.T) {
			called := false
			err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
				err := b.WithStatePage(ctx, srv.URL+path, navigationState(srv.URL, "saved"), PageOptions{WaitJS: "window.firstToken !== undefined"}, func(page *rod.Page) error {
					called = true
					want := "saved"
					if path == "/script" {
						want = "rotated"
					}
					got, err := page.Eval(`() => firstToken`)
					if err == nil && got.Value.Str() != want {
						return fmt.Errorf("redirect restored stale state: got %q, want %q", got.Value.Str(), want)
					}
					return err
				})
				return errors.Join(err, assertStatePagesClosed(b))
			})
			if path != "/cross" {
				if err != nil || !called {
					t.Fatalf("same-origin redirect: called=%v err=%v", called, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "origin mismatch") || called {
				t.Fatalf("cross-origin redirect: called=%v err=%v", called, err)
			}
			select {
			case got := <-seen:
				if got != "empty" {
					t.Fatalf("storage restored to cross origin: %q", got)
				}
			case <-ctx.Done():
				t.Fatal("cross-origin page did not report its initial storage")
			}
		})
	}
}

func TestWithStatePageCleanupAfterFailureCancelAndPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>ready</html>")
	}))
	defer srv.Close()
	pool := newLivePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, mode := range []string{"error", "cancel", "panic", "navigation", "wait", "pre-cancel"} {
		t.Run(mode, func(t *testing.T) {
			if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
				callCtx, stop := context.WithCancel(ctx)
				defer stop()
				want := errors.New("callback failed")
				path := "/"
				opts := PageOptions{Timeout: 3 * time.Second}
				if mode == "navigation" {
					path = "/slow"
					opts.Timeout = 250 * time.Millisecond
				}
				if mode == "wait" {
					opts.Timeout = 500 * time.Millisecond
					opts.Wait = 10 * time.Second
				}
				if mode == "pre-cancel" {
					stop()
				}
				func() {
					if mode == "panic" {
						defer func() {
							if recover() != "boom" {
								t.Error("callback panic lost")
							}
						}()
					}
					err := b.WithStatePage(callCtx, srv.URL+path, navigationState(srv.URL, "saved"), opts, func(page *rod.Page) error {
						switch mode {
						case "error":
							return want
						case "cancel":
							stop()
							_, err := page.Eval(`() => true`)
							return err
						case "panic":
							panic("boom")
						default:
							t.Error("callback reached after timeout/cancellation")
							return nil
						}
					})
					switch mode {
					case "error":
						if !errors.Is(err, want) {
							t.Errorf("callback error lost: %v", err)
						}
					case "cancel", "pre-cancel":
						if !errors.Is(err, context.Canceled) {
							t.Errorf("cancellation lost: %v", err)
						}
					case "navigation", "wait":
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Errorf("deadline lost: %v", err)
						}
					}
				}()
				return assertStatePagesClosed(b)
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertStatePagesClosed(b *Browser) error {
	targets, err := (proto.TargetGetTargets{}).Call(b.Browser)
	if err != nil {
		return err
	}
	for _, target := range targets.TargetInfos {
		if target.BrowserContextID == b.Browser.BrowserContextID && target.Type == "page" {
			return fmt.Errorf("state page leaked")
		}
	}
	return nil
}
