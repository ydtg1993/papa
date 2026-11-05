package browser

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func TestPageStateEncryptionAndTamper(t *testing.T) {
	state := &PageState{Version: 1, Origin: "https://example.com", LocalStorage: map[string]string{"token": "secret-token"}, Cookies: []*proto.NetworkCookie{{Name: "sid", Value: "secret-cookie", Domain: "example.com", Path: "/", Session: true, HTTPOnly: true}}}
	key := bytes.Repeat([]byte{7}, 32)
	var a, b bytes.Buffer
	if err := WritePageState(&a, state, key); err != nil {
		t.Fatal(err)
	}
	if err := WritePageState(&b, state, key); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes(), b.Bytes()) || bytes.Contains(a.Bytes(), []byte("secret")) {
		t.Fatal("nonce reuse or plaintext")
	}
	got, err := ReadPageState(bytes.NewReader(a.Bytes()), key)
	if err != nil || got.LocalStorage["token"] != "secret-token" || !got.Cookies[0].HTTPOnly {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := ReadPageState(bytes.NewReader(a.Bytes()), bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("wrong key accepted")
	}
	tampered := bytes.Clone(a.Bytes())
	tampered[len(tampered)-1] ^= 1
	for _, data := range [][]byte{tampered, a.Bytes()[:10], append(bytes.Clone(a.Bytes()), 1)} {
		if _, err := ReadPageState(bytes.NewReader(data), key); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	if err := WritePageState(io.Discard, state, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPageStateValidation(t *testing.T) {
	for _, state := range []*PageState{nil, {Version: 2}, {Version: 1, Origin: "https://example.com/path"}, {Version: 1, Origin: "https://example.com", Cookies: []*proto.NetworkCookie{{Domain: "evil.com"}}}, {Version: 1, Origin: "https://example.com", Cookies: []*proto.NetworkCookie{{Domain: "example.com", Path: "/", PartitionKeyOpaque: true}}}} {
		var out bytes.Buffer
		if err := WritePageState(&out, state, make([]byte, 32)); err == nil || out.Len() != 0 {
			t.Fatalf("invalid state written: %v", err)
		}
	}
}

func TestPageStateInputLimitAndCookiePath(t *testing.T) {
	if _, err := ReadPageState(bytes.NewReader(bytes.Repeat([]byte{0}, maxStateBytes+100)), make([]byte, 32)); err == nil {
		t.Fatal("oversized input accepted")
	}
	state := &PageState{Version: 1, Origin: "https://example.com", Cookies: []*proto.NetworkCookie{{Domain: "example.com", Path: ".evil.com"}}}
	if err := validatePageState(state); err == nil {
		t.Fatal("cookie path can alter origin")
	}
}

func TestPageStateLiveRestoreAndOriginGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "account-a", Path: "/", HttpOnly: true, MaxAge: 3600})
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>login</html>")
	}))
	defer srv.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "other") }))
	defer other.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := newLivePool(t)
	var saved bytes.Buffer
	key := bytes.Repeat([]byte{1}, 32)
	if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		return b.WithNavigatedPage(ctx, srv.URL+"/login", PageOptions{}, func(page *rod.Page) error {
			if _, err := page.Eval(`() => localStorage.setItem('token','account-a')`); err != nil {
				return err
			}
			state, err := ExportPageState(page)
			if err != nil {
				return err
			}
			return WritePageState(&saved, state, key)
		})
	}); err != nil {
		t.Fatal(err)
	}
	state, err := ReadPageState(&saved, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		if err := b.WithNavigatedPage(ctx, other.URL, PageOptions{}, func(page *rod.Page) error {
			if err := RestorePageState(page, state); err == nil {
				t.Error("cross-origin state accepted")
			}
			return nil
		}); err != nil {
			return err
		}
		return b.WithNavigatedPage(ctx, srv.URL+"/account", PageOptions{}, func(page *rod.Page) error {
			if _, err := page.Eval(`() => localStorage.setItem('stale','remove-me')`); err != nil {
				return err
			}
			if err := RestorePageState(page, state); err != nil {
				return err
			}
			if err := page.Reload(); err != nil {
				return err
			}
			if err := page.WaitLoad(); err != nil {
				return err
			}
			result, err := page.Eval(`() => localStorage.getItem('token')+'|'+localStorage.getItem('stale')`)
			if err != nil {
				return err
			}
			if result.Value.Str() != "account-a|null" {
				t.Errorf("storage: %s", result.Value.Str())
			}
			cookies, err := page.Cookies(nil)
			if err != nil {
				return err
			}
			if len(cookies) != 1 || cookies[0].Value != "account-a" || !cookies[0].HTTPOnly {
				t.Errorf("cookies: %+v", cookies)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		return b.WithNavigatedPage(ctx, srv.URL, PageOptions{}, func(p *rod.Page) error {
			if _, err := ExportPageState(p); err == nil {
				t.Error("default context exported")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}
