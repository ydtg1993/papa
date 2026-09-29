package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ydtg1993/papa/v2/crawler"
)

func TestWebhookNotify(t *testing.T) {
	var got crawler.AlertEvent
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	event := crawler.AlertEvent{
		Level: crawler.AlertError,
		TaskError: crawler.TaskError{
			Stage: "detail", TaskID: 3, URL: "http://x", Retry: 2, Kind: "structure", Message: "boom",
		},
	}
	if err := NewWebhook(srv.URL).Notify(context.Background(), event); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %s", method)
	}
	if got.Level != crawler.AlertError || got.Stage != "detail" || got.TaskID != 3 || got.Kind != "structure" {
		t.Fatalf("unexpected event: %+v", got)
	}
}

func TestWebhookErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := NewWebhook(srv.URL).Notify(context.Background(), crawler.AlertEvent{}); err == nil {
		t.Fatal("expected error for 500 status")
	}
}

func TestWebhookEmptyURL(t *testing.T) {
	if err := (&Webhook{}).Notify(context.Background(), crawler.AlertEvent{}); err == nil {
		t.Fatal("expected error for empty url")
	}
}
