package browser

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

	"github.com/ydtg1993/papa/v4/core"
)

func TestCaptureRequestIncludeHTMLCurrentDOMSingleNavigation(t *testing.T) {
	var documents atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/page" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		documents.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><div id="evidence">original</div><script>
document.getElementById('evidence').textContent='rendered-evidence';
setTimeout(function(){fetch('/matched')},100);
</script></body></html>`)
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	for _, include := range []bool{false, true} {
		err := pool.WithBrowser(context.Background(), func(b *Browser) error {
			res, err := b.CaptureRequest(context.Background(), srv.URL+"/page", CaptureOptions{
				Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, "/matched") },
				CaptureTimeout: 5 * time.Second, IncludeHTML: include,
			})
			if err != nil {
				return err
			}
			if res == nil || res.Request == nil {
				return fmt.Errorf("missing captured request: %+v", res)
			}
			if include && !strings.Contains(res.HTML, `<div id="evidence">rendered-evidence</div>`) {
				return fmt.Errorf("current DOM missing: %q", res.HTML)
			}
			if !include && res.HTML != "" {
				return fmt.Errorf("default unexpectedly captured HTML")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if documents.Load() != 2 {
		t.Fatalf("unexpected extra navigation: %d", documents.Load())
	}
}

func TestCaptureRequestIncludeHTMLSkipsChallengeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("cf-mitigated", "challenge")
		_, _ = fmt.Fprint(w, `<html><body>challenge-body<script>setTimeout(function(){fetch('/matched')},100)</script></body></html>`)
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		res, err := b.CaptureRequest(context.Background(), srv.URL, CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, "/matched") },
			CaptureTimeout: 5 * time.Second, IncludeHTML: true, WaitLoad: true,
		})
		if !errors.Is(err, core.ErrSiteChallenge) {
			return fmt.Errorf("challenge not classified: %v", err)
		}
		if res == nil || res.Request == nil || res.HTML != "" {
			return fmt.Errorf("challenge success snapshot retained: %+v", res)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
