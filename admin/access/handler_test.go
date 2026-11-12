package access

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type silentLogger struct{}

func (silentLogger) Info(...any)           {}
func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}
func TestWhitelistPersistenceAndAuthenticatedRoutes(t *testing.T) {
	h := New(Options{Whitelist: []string{"127.0.0.1"}, SaveWhitelist: func([]string, string) error { return errors.New("unavailable") }}, silentLogger{})
	r := httptest.NewRequest("POST", "/api/settings/whitelist", strings.NewReader(`{"whitelist":["192.0.2.1"]}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.Whitelist(w, r)
	if w.Code != 500 || !h.IPAllowed(r) {
		t.Fatal("failed persistence changed effective whitelist", w.Code)
	}
	h.SetWhitelist([]string{"192.0.2.1"})
	if h.IPAllowed(r) {
		t.Fatal("replacement not applied")
	}
	h.SetWhitelist(nil)
	w = httptest.NewRecorder()
	h.Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized request reached handler") })).ServeHTTP(w, r)
	if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
}
