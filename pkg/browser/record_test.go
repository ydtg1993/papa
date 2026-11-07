package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestRecordNetworkExchangesRedirectsAndSample(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<button id="go" onclick="fetch('/api',{method:'POST',body:'secret-input'})">go</button><script>fetch('/redirect');fetch('/slow')</script>`)
		case "/redirect":
			http.Redirect(w, r, "/final", 302)
		case "/slow":
			<-r.Context().Done()
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		record, err := b.RecordNetwork(context.Background(), srv.URL, RecordOptions{ObserveFor: 300 * time.Millisecond, Timeout: 5 * time.Second, Page: PageOptions{Interact: func(ctx context.Context, p *rod.Page) error {
			_, err := p.Eval(`() => document.querySelector('#go').click()`)
			return err
		}}})
		if err != nil {
			return err
		}
		var api, redirect, slow bool
		for _, e := range record.Entries {
			switch {
			case strings.HasSuffix(e.Request.URL, "/api"):
				api = true
				if !e.Completed || string(e.RequestBody) != "secret-input" || e.BodyOmitted || e.Request.Method != "POST" {
					t.Fatalf("API: %+v", e)
				}
				var sample bytes.Buffer
				if err := e.WriteSample(&sample, htmlfetch.SamplePolicy{}); err != nil {
					return err
				}
				if strings.Contains(sample.String(), "secret-input") {
					t.Fatal("sample leaked input")
				}
			case strings.HasSuffix(e.Request.URL, "/redirect"):
				redirect = e.Redirect && e.StatusCode == 302
			case strings.HasSuffix(e.Request.URL, "/slow"):
				slow = !e.Completed
				if err := e.WriteSample(io.Discard, htmlfetch.SamplePolicy{}); err == nil {
					t.Fatal("incomplete sample accepted")
				}
			}
		}
		if !api || !redirect || !slow {
			t.Fatalf("missing recording: api=%v redirect=%v slow=%v %+v", api, redirect, slow, record)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordNetworkBudgetsAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<script>for(let i=0;i<6;i++)fetch('/api/'+i)</script>`)
		} else {
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		}
	}))
	defer srv.Close()
	pool := newLivePool(t, srv.URL)
	if err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		r, err := b.RecordNetwork(context.Background(), srv.URL, RecordOptions{MaxEntries: 2, MaxTotalBodySize: 10, ObserveFor: 200 * time.Millisecond, Timeout: 5 * time.Second})
		if err != nil {
			return err
		}
		if len(r.Entries) > 2 || r.Dropped == 0 {
			t.Fatal(r)
		}
		total := 0
		for _, e := range r.Entries {
			total += len(e.Body) + len(e.RequestBody)
		}
		if total > 10 {
			t.Fatal("body budget exceeded")
		}
		_, err = b.RecordNetwork(context.Background(), srv.URL, RecordOptions{Timeout: 100 * time.Millisecond, ObserveFor: time.Second})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancel: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
