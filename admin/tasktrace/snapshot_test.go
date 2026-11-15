package tasktrace

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSnapshotDownloadContract(t *testing.T) {
	payload := []byte("<script>unsafe()</script>\x00\xff")
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request")
	calls := 0
	h := New(Options{Snapshot: func(got context.Context, token string) ([]byte, error) {
		calls++
		if got.Value(contextKey{}) != "request" {
			t.Error("request context lost")
		}
		if token == "opaque-token" {
			return payload, nil
		}
		return nil, errors.New("private disk path or invalid token detail")
	}})
	for _, tc := range []struct {
		name, method, query string
		status, calls       int
	}{
		{"download", "GET", "?file=opaque-token", 200, 1},
		{"method", "POST", "?file=opaque-token", 405, 0},
		{"missing", "GET", "", 400, 0},
		{"blank", "GET", "?file=%20", 400, 0},
		{"invalid", "GET", "?file=../old/path.html", 404, 1},
		{"expired", "GET", "?file=expired-token", 404, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			w := httptest.NewRecorder()
			h.Page(w, httptest.NewRequest(tc.method, "/api/task/page"+tc.query, nil).WithContext(ctx))
			if w.Code != tc.status || calls-before != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls-before, w.Body.String())
			}
			if tc.status == 200 {
				if !bytes.Equal(w.Body.Bytes(), payload) {
					t.Fatal("snapshot bytes changed")
				}
				if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") != `attachment; filename="snapshot.bin"` || w.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("headers=%v", w.Header())
				}
			}
			if bytes.Contains(w.Body.Bytes(), []byte("private disk")) {
				t.Fatal("internal error disclosed")
			}
		})
	}
	w := httptest.NewRecorder()
	New(Options{}).Page(w, httptest.NewRequest(http.MethodGet, "/api/task/page?file=opaque-token", nil))
	if w.Code != 404 {
		t.Fatalf("disabled status=%d", w.Code)
	}
}
