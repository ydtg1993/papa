package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestAccountPoolStandardHTTPHelpersKeepIdentityAndCacheScope(t *testing.T) {
	var conditional atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			t.Error("anonymous pool request")
		}
		w.Header().Set("ETag", `"same-tag"`)
		if r.Header.Get("If-None-Match") != "" {
			conditional.Add(1)
			w.WriteHeader(304)
			return
		}
		fmt.Fprint(w, "<h1>"+token+"</h1>")
	}))
	defer srv.Close()
	e, _, _ := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	m := account.NewManager(client)
	for _, id := range []string{"a", "b"} {
		id := id
		if err := m.Register(account.Key{Site: "s", ID: id}, account.Config{Origins: []string{srv.URL}, CheckInterval: time.Hour, Login: func(_ context.Context, c *account.Credentials) error { c.Headers["Authorization"] = id; return nil }, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := account.NewPools(context.Background(), m, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e.SetAccountManager(m)
	key := account.PoolKey{Site: "s", Name: "p"}
	if _, err = p.Upsert(context.Background(), account.PoolConfig{Key: key, Members: []string{"a", "b"}, Cooldown: time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a, err := p.Acquire(deadline, key, account.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	ar := e.newArchiveBuffer(&Task{ID: 1, Site: "s", Stage: "detail"}, 0)
	ctx := account.WithAttempt(withArchive(deadline, ar), a)
	page, err := e.FetchHTML(ctx, srv.URL)
	if err != nil || !strings.Contains(page.HTML, "<h1>a</h1>") {
		t.Fatalf("page=%v err=%v", page, err)
	}
	if _, err = e.FetchHTTP(ctx, htmlfetch.Request{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if len(ar.pages) != 0 {
		t.Fatal("bound account HTML entered raw archive")
	}
	first, err := e.FetchIncremental(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.FetchIncremental(ctx, srv.URL, &first.Representation)
	if err != nil || !second.NotModified {
		t.Fatalf("same-account validator failed: %v", err)
	}
	p.ReportFailure(a, account.Expired, 0)
	b, err := p.Acquire(deadline, key, account.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	third, err := e.FetchIncremental(account.WithAttempt(deadline, b), srv.URL, &first.Representation)
	if err != nil || third.NotModified || conditional.Load() != 1 {
		t.Fatalf("validator crossed account switch: result=%+v err=%v", third, err)
	}
	if _, err = e.CaptureRequest(ctx, srv.URL, browser.CaptureOptions{}); err == nil {
		t.Fatal("capture silently bypassed account")
	}
	if _, err = e.CaptureResponse(ctx, srv.URL, browser.ResponseOptions{}); err == nil {
		t.Fatal("response capture silently bypassed account")
	}
}
