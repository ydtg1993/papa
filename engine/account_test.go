package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestAccountEngineScopeTraceAndNoRawArchive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing auth")
		}
		_, _ = io.WriteString(w, "<h1>private-body</h1>")
	}))
	defer srv.Close()
	e, _, _ := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	e.fetchResources.SetHTML(htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}))
	m := account.NewManager(e.GetHTMLClient())
	key := account.Key{Site: "site", ID: "alice"}
	if err := m.Register(key, account.Config{Origins: []string{srv.URL}, Login: func(_ context.Context, c *account.Credentials) error {
		c.Headers["Authorization"] = "Bearer private-token"
		return nil
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	e.SetAccountManager(m)
	task := &Task{ID: 1, Site: "site", Stage: "detail"}
	archive := e.newArchiveBuffer(task, 0)
	trace := newTrace(nil, 1, 0)
	ctx := withTrace(withArchive(context.Background(), archive), trace)
	if _, err := e.FetchAccountHTTP(ctx, key, htmlfetch.Request{URL: srv.URL + "?token=private-url"}); err != nil {
		t.Fatal(err)
	}
	p, err := e.FetchAccountHTML(ctx, key, srv.URL)
	if err != nil || !strings.Contains(p.HTML, "private-body") {
		t.Fatal(p, err)
	}
	if len(archive.pages) != 0 {
		t.Fatal("account HTML entered raw archive")
	}
	b, _ := json.Marshal(trace.flush())
	if strings.Contains(string(b), "private-") {
		t.Fatal("trace leaked credentials")
	}
	b, _ = json.Marshal(e.AccountStatus())
	if strings.Contains(string(b), "private-") {
		t.Fatal("status leaked credentials")
	}
	e.SetAccountManager(nil)
	if _, err = e.FetchAccountHTTP(ctx, key, htmlfetch.Request{URL: srv.URL}); err == nil {
		t.Fatal("missing manager accepted")
	}
}
