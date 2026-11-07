package extract_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/extract"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// Exercise the actual API -> validator -> sanitized fixture -> offline parser
// boundary, then a conditional request after the successful representation commit.
func TestIncrementalValidatedSamplePipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		_, _ = io.WriteString(w, `[{"id":9007199254740993,"title":"中文标题","token":"private"}]`)
	}))
	defer srv.Close()
	client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	r, err := client.FetchIncremental(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := r.Response.DecodeJSON(&rows); err != nil {
		t.Fatal(err)
	}
	v, err := extract.NewValidator([]extract.FieldRule{{Pointer: "/id", Kind: "integer", Required: true, Unique: true}, {Pointer: "/title", Kind: "string", Required: true, MinLength: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if report := v.Validate(rows, 10); report.InvalidRows != 0 {
		t.Fatal(report)
	}
	var sample bytes.Buffer
	err = htmlfetch.WriteSample(&sample, htmlfetch.Request{URL: srv.URL}, r.Response, htmlfetch.SamplePolicy{RedactBody: func(request bool, body []byte) ([]byte, error) {
		if request {
			return nil, nil
		}
		return bytes.ReplaceAll(body, []byte(`,"token":"private"`), nil), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := htmlfetch.ReadSample(&sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	offline, err := fixture.OfflineResponse()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(offline.Body, []byte(`"token"`)) || bytes.Contains(offline.Body, []byte("private")) {
		t.Fatal("fixture body retained credential")
	}
	// Decode into a fresh result: encoding/json retains absent keys when maps
	// from an earlier decode are reused.
	var offlineRows []map[string]any
	if err := offline.DecodeJSON(&offlineRows); err != nil {
		t.Fatal(err)
	}
	if len(offlineRows) != 1 {
		t.Fatalf("offline rows = %d, want 1", len(offlineRows))
	}
	if _, exists := offlineRows[0]["token"]; exists {
		t.Fatal("fixture retained credential")
	}
	if report := v.Validate(offlineRows, 10); report.InvalidRows != 0 {
		t.Fatal(report)
	}
	committed := r.Representation
	r, err = client.FetchIncremental(context.Background(), srv.URL, &committed)
	if err != nil || !r.NotModified || r.Changed {
		t.Fatal(r, err)
	}
}
