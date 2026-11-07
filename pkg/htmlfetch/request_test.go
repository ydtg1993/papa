package htmlfetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestDoPostHeadersAndJSONPrecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != `{"page":2}` || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Value") != "request" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request: %s %s %v", r.Method, body, r.Header)
		}
		w.Header().Add("X-Reply", "one")
		w.Header().Add("X-Reply", "two")
		_, _ = io.WriteString(w, `{"id":9007199254740993}`)
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, Headers: map[string]string{"X-Value": "config", "Authorization": "secret"}})
	ctx := core.WithHeaders(context.Background(), map[string]string{"X-Value": "context", "authorization": ""})
	r, err := c.Do(ctx, Request{URL: srv.URL, Method: "POST", Headers: map[string]string{"Content-Type": "application/json", "X-Value": "request"}, Body: []byte(`{"page":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := r.DecodeJSON(&data); err != nil {
		t.Fatal(err)
	}
	if data["id"] != json.Number("9007199254740993") || len(r.Header.Values("X-Reply")) != 2 {
		t.Fatalf("response: %+v %+v", r, data)
	}
}

func TestDoErrorBodyLimitAndRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/form", http.StatusTemporaryRedirect)
		case "/form":
			if err := r.ParseForm(); err != nil || r.Method != "POST" || r.Form.Get("q") != "中文" {
				t.Errorf("form replay: %s %v %v", r.Method, r.Form, err)
			}
			_, _ = io.WriteString(w, "ok")
		case "/error":
			w.WriteHeader(429)
			_, _ = io.WriteString(w, "slow")
		default:
			_, _ = io.WriteString(w, "12345")
		}
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, MaxBodySize: 4})
	r, err := c.Do(context.Background(), Request{URL: srv.URL + "/error"})
	if code, ok := StatusCode(err); !ok || code != 429 || string(r.Body) != "slow" {
		t.Fatalf("error response: %+v %v", r, err)
	}
	r, err = c.Do(context.Background(), Request{URL: srv.URL})
	var large *BodyTooLargeError
	if !errors.As(err, &large) || r.Body != nil {
		t.Fatalf("limit: %+v %v", r, err)
	}
	r, err = c.Do(context.Background(), Request{URL: srv.URL + "/redirect", Method: "POST", Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, Body: []byte(url.Values{"q": {"中文"}}.Encode())})
	if err != nil || r.URL.Path != "/form" || string(r.Body) != "ok" {
		t.Fatalf("redirect: %+v %v", r, err)
	}
}

func TestDoCanceledAndHotConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	c.SetConfig(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, Timeout: 20 * time.Millisecond})
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Do(ctx, Request{URL: srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestSessionsIsolateConcurrentAccountsAndDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if account := r.URL.Query().Get("account"); account != "" {
			http.SetCookie(w, &http.Cookie{Name: "account", Value: account, Path: "/"})
			return
		}
		cookie, err := r.Cookie("account")
		if err == nil {
			_, _ = io.WriteString(w, cookie.Value)
		}
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	var wg sync.WaitGroup
	for _, account := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			ctx := WithSession(context.Background(), NewSession())
			if _, err := c.Do(ctx, Request{URL: srv.URL + "?account=" + account}); err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 5; i++ {
				r, err := c.Do(ctx, Request{URL: srv.URL})
				if err != nil {
					t.Error(err)
					return
				}
				if string(r.Body) != account {
					t.Errorf("session got %q want %q", r.Body, account)
				}
			}
			r, err := c.Do(WithSession(ctx, nil), Request{URL: srv.URL})
			if err != nil {
				t.Error(err)
				return
			}
			if len(r.Body) != 0 {
				t.Errorf("nil session leaked %q", r.Body)
			}
		}(account)
	}
	wg.Wait()
	r, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err != nil || len(r.Body) != 0 {
		t.Fatalf("default session: %+v %v", r, err)
	}
}

func TestSessionCookieScope(t *testing.T) {
	s := NewSession()
	origin, _ := url.Parse("https://a.example.com/private/page")
	s.SetCookies(origin, []*http.Cookie{{Name: "scoped", Value: "yes", Path: "/private", Secure: true}, {Name: "bad", Value: "no", Domain: "com", Path: "/"}})
	if got := s.Cookies(origin); len(got) != 1 || got[0].Name != "scoped" {
		t.Fatalf("cookies: %v", got)
	}
	for _, raw := range []string{"https://b.example.com/private/page", "http://a.example.com/private/page", "https://a.example.com/other"} {
		u, _ := url.Parse(raw)
		if len(s.Cookies(u)) != 0 {
			t.Fatalf("cookie leaked to %s", raw)
		}
	}
}

func TestJSONBlocksAndStrictDecode(t *testing.T) {
	p, err := NewRenderedPage("https://example.com", `<script type="application/ld+json">{"id":9007199254740993}</script><script type="application/ld+json">[1,2]</script><script id="js">window.x={};</script>`)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := p.Document.JSONBlocks(`script[type="application/ld+json"]`)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("blocks: %v %v", blocks, err)
	}
	for _, selector := range []string{"[", ".missing", "#js"} {
		if _, err := p.Document.JSONBlocks(selector); err == nil {
			t.Errorf("accepted %q", selector)
		}
	}
	for _, raw := range []string{`{} {}`, `{} trailing`, ``} {
		var dst any
		if err := DecodeJSON([]byte(raw), &dst); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	var dst any
	if err := DecodeJSON([]byte(strings.TrimSpace(string(blocks[0]))), &dst); err != nil {
		t.Fatal(err)
	}
}
