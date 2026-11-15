package htmlfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestFetchWithResponseOriginalBytesSingleRequest(t *testing.T) {
	for _, status := range []int{200, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := append([]byte("<h1>original</h1>"), 0xff, 0, 0xfe)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(status)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
			page, resp, err := c.FetchWithResponse(context.Background(), srv.URL)
			if calls.Load() != 1 || resp == nil || resp.StatusCode != status || !bytes.Equal(resp.Body, body) {
				t.Fatalf("calls=%d response=%+v error=%v", calls.Load(), resp, err)
			}
			if status == 200 {
				if err != nil || page == nil || page.HTML != string(body) || page.Document == nil || page.URL.String() != resp.URL.String() || page.ContentType != resp.Header.Get("Content-Type") {
					t.Fatalf("page=%+v error=%v", page, err)
				}
			} else {
				var statusErr *StatusError
				if page != nil || !errors.As(err, &statusErr) || statusErr.Code != status {
					t.Fatalf("page=%+v error=%v", page, err)
				}
			}
		})
	}
}

func TestFetchWithResponseOversizeAndReadFailure(t *testing.T) {
	for _, mode := range []string{"oversize", "read-failure"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "read-failure" {
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(404)
				_, _ = w.Write([]byte("123456789"))
			}))
			defer srv.Close()
			maxBody := int64(4)
			if mode == "read-failure" {
				maxBody = 100
			}
			c := NewClient(Config{MaxBodySize: maxBody, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
			page, resp, err := c.FetchWithResponse(context.Background(), srv.URL)
			if page != nil || resp == nil || len(resp.Body) != 0 || err == nil {
				t.Fatalf("page=%+v response=%+v error=%v", page, resp, err)
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) || statusErr.Code != 404 {
				t.Fatalf("status error lost: %v", err)
			}
			var large *BodyTooLargeError
			if mode == "oversize" && !errors.As(err, &large) {
				t.Fatalf("size error lost: %v", err)
			}
		})
	}
}

func TestFetchWithResponseChallengeAndInvalidRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(403)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClient(Config{Timeout: time.Second, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	page, resp, err := c.FetchWithResponse(context.Background(), srv.URL)
	if page != nil || resp == nil || len(resp.Body) != 0 || !errors.Is(err, core.ErrSiteChallenge) {
		t.Fatalf("page=%+v response=%+v error=%v", page, resp, err)
	}
	page, resp, err = c.FetchWithResponse(context.Background(), "")
	if page != nil || resp != nil || err == nil {
		t.Fatalf("invalid URL accepted: %+v %+v %v", page, resp, err)
	}
}
