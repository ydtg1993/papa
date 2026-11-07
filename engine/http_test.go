package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func TestFetchHTTPTraceExcludesCredentialsAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid=secret-cookie")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "secret-body")
	}))
	defer srv.Close()
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	trace := newTrace(e.db, logrus.New(), 1, 0)
	r, err := e.FetchHTTP(withTrace(context.Background(), trace), htmlfetch.Request{URL: srv.URL + "?token=secret-url", Headers: map[string]string{"Authorization": "secret-header"}})
	if err == nil || r == nil {
		t.Fatalf("expected error response: %+v %v", r, err)
	}
	records := trace.flush()
	raw, err := json.Marshal(records)
	if err != nil || len(records) != 1 || strings.Contains(string(raw), "secret-") {
		t.Fatalf("trace: %s %v", raw, err)
	}
	data := decodeTraceData(t, records[0])
	if data["status_code"] != float64(401) {
		t.Fatalf("missing status: %v", data)
	}
}

func TestFetchHTTPDisabledAndContextHeaders(t *testing.T) {
	e := &Engine{}
	if _, err := e.FetchHTTP(context.Background(), htmlfetch.Request{URL: "https://example.com"}); err == nil {
		t.Fatal("disabled should fail")
	}
	if _, err := e.CaptureResponse(context.Background(), "https://example.com", browser.ResponseOptions{}); err == nil {
		t.Fatal("disabled browser should fail")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Header.Get("X-Site")) }))
	defer srv.Close()
	e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	r, err := e.FetchHTTP(core.WithHeaders(context.Background(), map[string]string{"X-Site": "site-a"}), htmlfetch.Request{URL: srv.URL})
	if err != nil || string(r.Body) != "site-a" {
		t.Fatalf("response: %+v %v", r, err)
	}
}
