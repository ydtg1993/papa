package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func TestIsolatedBrowserStorageAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>account</body></html>")
		if r.URL.Path == "/isolated" && (r.Header.Get("Authorization") != "" || r.Header.Get("X-Shared") != "") {
			t.Errorf("shared credentials inherited: %v", r.Header)
		}
	}))
	defer srv.Close()
	pool := newLivePool(t)
	pool.SetHeaders(map[string]string{"Authorization": "shared-secret", "X-Shared": "global"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	visit := func(b *Browser, path, want, set string) error {
		return b.WithNavigatedPage(ctx, srv.URL+path, PageOptions{Timeout: 5 * time.Second}, func(page *rod.Page) error {
			r, err := page.Eval(`() => (localStorage.getItem('account') || '') + '|' + document.cookie`)
			if err != nil {
				return err
			}
			if r.Value.Str() != want {
				return fmt.Errorf("storage %q want %q", r.Value.Str(), want)
			}
			if set != "" {
				_, err = page.Eval(`(v) => { localStorage.setItem('account',v); document.cookie='account='+v+';path=/'; }`, set)
			}
			return err
		})
	}
	var parentPage *rod.Page
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		var err error
		parentPage, err = b.NewPageWithOptions(ctx, srv.URL+"/parent", PageOptions{})
		if err != nil {
			return err
		}
		_, err = parentPage.Eval(`() => {localStorage.setItem('account','parent');document.cookie='account=parent;path=/';}`)
		b.defaultCookies = []*proto.NetworkCookieParam{{Name: "default", Value: "secret", URL: srv.URL}}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer parentPage.Close()

	for _, account := range []string{"alice", "bob"} {
		if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
			if b.Browser.BrowserContextID == "" {
				return fmt.Errorf("missing isolated context")
			}
			if err := visit(b, "/isolated", "|", account); err != nil {
				return err
			}
			return visit(b, "/isolated", account+"|account="+account, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		b.defaultCookies = nil
		return visit(b, "/parent", "parent|account=parent", "")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIsolatedBrowserCleanupOnErrorCancelAndPanic(t *testing.T) {
	pool := newLivePool(t)
	for _, mode := range []string{"error", "cancel", "panic", "disposed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var id proto.BrowserBrowserContextID
			want := errors.New("callback failed")
			func() {
				if mode == "panic" {
					defer func() {
						if recover() != "boom" {
							t.Error("panic was not propagated")
						}
					}()
				}
				err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
					id = b.Browser.BrowserContextID
					// Deliberately leave a page open: context disposal must close it too.
					if _, err := b.newPageBase(PageOptions{}); err != nil {
						return err
					}
					switch mode {
					case "disposed":
						return errors.Join(want, b.Browser.Close())
					case "panic":
						panic("boom")
					case "cancel":
						cancel()
						return ctx.Err()
					default:
						return want
					}
				})
				if (mode == "error" || mode == "disposed") && !errors.Is(err, want) {
					t.Errorf("error lost: %v", err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Errorf("cancel lost: %v", err)
				}
			}()
			if err := pool.WithBrowser(context.Background(), func(b *Browser) error {
				contexts, err := (proto.TargetGetBrowserContexts{}).Call(b.Browser)
				if err != nil {
					return err
				}
				for _, got := range contexts.BrowserContextIDs {
					if got == id {
						return fmt.Errorf("context leaked")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
