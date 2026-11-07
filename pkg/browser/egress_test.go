package browser

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
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestEgressNavigationRejectsLocalFilesBeforeBrowserAccess(t *testing.T) {
	b := &Browser{}
	for _, raw := range []string{"file:///private.txt", "data:text/html,secret", "javascript:alert(1)"} {
		_, err := b.NewPageWithOptions(context.Background(), raw, PageOptions{})
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
		_, err = b.FetchOnce(context.Background(), raw, PageOptions{})
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
		_, err = b.CaptureRequest(context.Background(), raw, CaptureOptions{})
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
		_, err = b.CaptureResponse(context.Background(), raw, ResponseOptions{})
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
		_, err = b.RecordNetwork(context.Background(), raw, RecordOptions{})
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
		err = b.WithStatePage(context.Background(), raw, nil, PageOptions{}, nil)
		if !errors.Is(err, egress.ErrDenied) {
			t.Fatal(err)
		}
	}
}
func TestEgressLauncherCannotDisableGateway(t *testing.T) {
	l := launcher.New().Set(flags.Flag("no-proxy-server")).Set(flags.Flag("proxy-bypass-list"), "*").Set(flags.Flag("proxy-pac-url"), "http://bad/")
	secureLauncher(l, "http://127.0.0.1:12345")
	if l.Has(flags.Flag("no-proxy-server")) || l.Has(flags.Flag("proxy-pac-url")) || l.Get(flags.Flag("proxy-bypass-list")) != "<-loopback>" || !strings.Contains(l.Get(flags.Flag("host-resolver-rules")), "~NOTFOUND") {
		t.Fatal("browser gateway could be bypassed")
	}
}
func TestEgressRealBrowserResourcesRedirectAndPopup(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var blocked, allowed atomic.Int32
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { blocked.Add(1); fmt.Fprint(w, "private") }))
	defer denied.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			allowed.Add(1)
			fmt.Fprint(w, "ok")
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, denied.URL, 302)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>window.open(%q);Promise.allSettled([fetch('/ok'),fetch(%q),fetch('/redirect')]).then(()=>{document.body.innerHTML='<div id="done">done</div>'})</script></body></html>`, denied.URL, denied.URL)
	}))
	defer main.Close()
	p, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: []string{main.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = p.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.FetchOnce(ctx, main.URL, PageOptions{Timeout: 10 * time.Second, WaitSelector: "#done"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Load() != 1 || blocked.Load() != 0 {
		t.Fatalf("allowed=%d blocked destination hits=%d", allowed.Load(), blocked.Load())
	}
}
