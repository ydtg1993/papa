package htmlfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestIncrementalETag304AndHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		_, _ = io.WriteString(w, "data")
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	first, err := c.FetchIncremental(context.Background(), srv.URL, nil)
	if err != nil || !first.Changed {
		t.Fatalf("first: %+v %v", first, err)
	}
	next, err := c.FetchIncremental(context.Background(), srv.URL, &first.Representation)
	if err != nil || !next.NotModified || next.Changed || len(next.Response.Body) != 0 || next.Representation.SHA256 != ContentHash([]byte("data")) {
		t.Fatalf("next: %+v %v", next, err)
	}
	first.Representation.ETag = ""
	next, err = c.FetchIncremental(context.Background(), srv.URL, &first.Representation)
	if err != nil || next.NotModified || next.Changed {
		t.Fatalf("hash: %+v %v", next, err)
	}
	// Existing generic request semantics must still classify 304 as non-2xx.
	_, err = c.Do(context.Background(), Request{URL: srv.URL, Headers: map[string]string{"If-None-Match": `"v1"`}})
	if code, ok := StatusCode(err); !ok || code != 304 {
		t.Fatal(err)
	}
}

func TestIncrementalRedirectAndUnexpectedStatuses(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/target", 302)
		case "/target":
			followed = true
		case "/partial":
			w.WriteHeader(206)
		default:
			w.WriteHeader(304)
		}
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	for _, path := range []string{"/redirect", "/partial", "/unexpected"} {
		if _, err := c.FetchIncremental(context.Background(), srv.URL+path, nil); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	if followed {
		t.Fatal("conditional fetch followed redirect")
	}
}

func TestIncrementalLastModifiedAndURLScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/new" {
			if r.Header.Get("If-Modified-Since") != "" {
				t.Error("validator crossed URL")
			}
			_, _ = io.WriteString(w, "new")
			return
		}
		if r.Header.Get("If-Modified-Since") != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Error("missing last modified")
		}
		w.WriteHeader(304)
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	previous := &Representation{URL: srv.URL, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"}
	if r, err := c.FetchIncremental(context.Background(), srv.URL, previous); err != nil || !r.NotModified {
		t.Fatal(r, err)
	}
	if r, err := c.FetchIncremental(context.Background(), srv.URL+"/new", previous); err != nil || !r.Changed {
		t.Fatal(r, err)
	}
}
