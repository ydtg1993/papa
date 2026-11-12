package settings

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type silentLogger struct{}

func (silentLogger) Errorf(string, ...any) {}
func TestShutdownRequiresSecondCredentialAndSettingsSnapshot(t *testing.T) {
	calls := 0
	h := New(Options{Whitelist: func() []string { return []string{} }, WhitelistPersistent: true, LogDir: "logs", OnShutdown: func() { calls++ }, VerifyTokenValue: func(string) (string, bool) { return "", false }}, silentLogger{})
	w := httptest.NewRecorder()
	h.Shutdown(w, httptest.NewRequest("POST", "/api/settings/shutdown", strings.NewReader(`{"token":"invalid"}`)))
	if w.Code != 403 || calls != 0 {
		t.Fatal("invalid confirmation accepted", w.Code)
	}
	w = httptest.NewRecorder()
	h.Settings(w, httptest.NewRequest("GET", "/api/settings", nil))
	for _, want := range []string{`"whitelist":[]`, `"whitelist_source":"crawler_config.ip_whitelist"`, `"log_dir":"logs"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Body.String())
		}
	}
}
