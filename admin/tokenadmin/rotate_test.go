package tokenadmin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRotateAPIReturnsCurrentCredentialAndAuditsWithoutSecret(t *testing.T) {
	store, mux, events := newTestAPI()
	id := store.Seed("old", "operator", "note")
	req := httptest.NewRequest(http.MethodPost, "/api/tokens/rotate", strings.NewReader(fmt.Sprintf(`{"id":%d}`, id)))
	req.Header.Set("Authorization", "Bearer old")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := decode(t, w)
	token, _ := body["token"].(string)
	if w.Code != http.StatusOK || body["current"] != true || len(token) != 64 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d %v", w.Code, body)
	}
	if _, ok := store.Verify(token); !ok {
		t.Fatal("new credential rejected")
	}
	if len(*events) != 1 || (*events)[0].Action != "rotate" || (*events)[0].ID != id || len((*events)[0].Values) != 0 {
		t.Fatalf("unexpected audit: %+v", *events)
	}
	if got := do(t, mux, http.MethodGet, "/api/tokens/rotate", ""); got.Code != http.StatusMethodNotAllowed {
		t.Fatal("GET must not rotate")
	}
}

func TestRotateRevokesOldTokenAndKeepsIdentity(t *testing.T) {
	m := NewMemStore()
	id := m.Seed("old", "operator", "note")
	token, current, err := m.Rotate(id, "old")
	if err != nil || !current || token == "old" {
		t.Fatal("rotation failed", err)
	}
	if _, ok := m.Verify("old"); ok {
		t.Fatal("old token accepted")
	}
	if name, ok := m.Verify(token); !ok || name != "operator" {
		t.Fatal("new token rejected")
	}
	if err := m.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	token, current, err = m.Rotate(id, "unrelated")
	if err != nil || current {
		t.Fatal("rotation identity", err)
	}
	if _, ok := m.Verify(token); ok {
		t.Fatal("disabled token enabled by rotation")
	}
	rows, _ := m.List()
	if len(rows) != 1 || rows[0].Note != "note" {
		t.Fatal(rows)
	}
}
