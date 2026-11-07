package notify

import (
	"context"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookMasksBodyButPreservesSignedDestination(t *testing.T) {
	type received struct{ query, body string }
	ch := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- received{r.URL.RawQuery, string(b)}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	event := core.AlertEvent{TaskError: core.TaskError{URL: "https://example.test/?token=SECRET", Message: "password=SECRET"}}
	hook := NewWebhook(srv.URL + "?sign=SECRET&v=1")
	hook.Network = egress.Config{PrivateOrigins: []string{srv.URL}}
	err := hook.Notify(context.Background(), event)
	got := <-ch
	if got.query != "sign=SECRET&v=1" {
		t.Fatalf("destination signature changed: %s", got.query)
	}
	if strings.Contains(got.body, "SECRET") || err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("diagnostic leak: body=%s error=%v", got.body, err)
	}
	if !strings.Contains(event.URL, "SECRET") || !strings.Contains(hook.URL, "SECRET") {
		t.Fatal("operational input changed")
	}
}
