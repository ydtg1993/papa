package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func TestCaptureResponseXHRBodyLimitAndTimeout(t *testing.T) {
	const body = `{"items":[1,2]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<script>fetch('/noise');setTimeout(()=>fetch('/api',{method:'POST',body:'hello'}),40);</script>`)
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		match := func(r CapturedResponse) bool { return strings.HasSuffix(r.Request.URL, "/api") }
		res, err := b.CaptureResponse(context.Background(), srv.URL, ResponseOptions{Match: match, Timeout: 5 * time.Second, MaxBodySize: int64(len(body))})
		if err != nil {
			return err
		}
		if string(res.Body) != body || res.StatusCode != 422 || res.Request.Method != "POST" {
			t.Fatalf("response: %+v", res)
		}
		if string(res.RequestBody) != "hello" || res.RequestBodyIncomplete || res.RequestHeader.Get("Content-Type") == "" {
			t.Fatalf("request evidence: %+v", res)
		}
		res, err = b.CaptureResponse(context.Background(), srv.URL, ResponseOptions{Match: match, Timeout: 5 * time.Second, MaxRequestBodySize: 2})
		if err != nil || string(res.RequestBody) != "he" || !res.RequestBodyIncomplete {
			t.Fatalf("request limit: %+v %v", res, err)
		}
		res, err = b.CaptureResponse(context.Background(), srv.URL, ResponseOptions{Match: match, Timeout: 5 * time.Second, MaxBodySize: 2})
		var large *htmlfetch.BodyTooLargeError
		if !errors.As(err, &large) || res == nil || res.Body != nil {
			t.Fatalf("limit: %+v %v", res, err)
		}
		_, err = b.CaptureResponse(context.Background(), srv.URL, ResponseOptions{Match: func(CapturedResponse) bool { return false }, Timeout: 200 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout: %v", err)
		}
		pages, err := b.Browser.Pages()
		if err != nil {
			return err
		}
		for _, p := range pages {
			info, err := p.Info()
			if err == nil && strings.HasPrefix(info.URL, srv.URL) {
				t.Fatalf("capture leaked page: %s", info.URL)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBrowserCookieTransferPreservesScope(t *testing.T) {
	cookies, err := httpCookies([]*proto.NetworkCookie{{Name: "host", Value: "a", Domain: "a.example.com", Path: "/", Session: true}, {Name: "domain", Value: "b", Domain: ".example.com", Path: "/private", Secure: true, HTTPOnly: true, Expires: proto.TimeSinceEpoch(2000000000)}})
	if err != nil || cookies[0].Domain != "" || cookies[1].Domain != ".example.com" || !cookies[1].Secure || cookies[1].Expires.Unix() != 2000000000 {
		t.Fatalf("conversion: %+v %v", cookies, err)
	}
	if _, err := httpCookies([]*proto.NetworkCookie{{PartitionKeyOpaque: true}}); err == nil {
		t.Fatal("partitioned cookie should fail")
	}
}

func TestBrowserCookieTransferLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "account-a", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, "ok")
			return
		}
		cookie, err := r.Cookie("sid")
		if err == nil {
			_, _ = io.WriteString(w, cookie.Value)
		}
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	session := htmlfetch.NewSession()
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		p, err := b.newPageBase(PageOptions{})
		if err != nil {
			return err
		}
		defer p.Close()
		p = p.Timeout(5 * time.Second)
		if err := p.Navigate(srv.URL + "/login"); err != nil {
			return err
		}
		if err := p.WaitLoad(); err != nil {
			return err
		}
		return CopyCookiesToSession(p, session, srv.URL)
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}).Do(htmlfetch.WithSession(context.Background(), session), htmlfetch.Request{URL: srv.URL})
	if err != nil || string(r.Body) != "account-a" {
		t.Fatalf("cookie replay: %+v %v", r, err)
	}
}
