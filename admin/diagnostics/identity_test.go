package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/internal/browserdiagnostics"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

func diagnosticTestProfile() identity.Profile {
	return identity.Profile{Site: "fixture", ID: "desktop", Version: 1, SchemaVersion: 1, UAPolicy: identity.NativeChromium, Languages: []string{"en-US", "en"}, Locale: "en-US", Timezone: "UTC", Viewport: identity.Size{Width: 1280, Height: 720}, Screen: identity.Size{Width: 1920, Height: 1080}, DeviceScaleFactor: 1, Adapter: identity.AdapterConfig{ID: "webdriver", Version: "1", Enabled: true}}
}
func identityTestHandler(t *testing.T) *Handler {
	t.Helper()
	a := &Handler{Service: &browserdiagnostics.Service{}, Sites: func() any { return map[string]any{} }}
	a.Service.(*browserdiagnostics.Service).Configure(browserdiagnostics.Options{HasSite: func(site string) bool { return site == "fixture" }})
	m, err := identityruntime.NewManager(identityruntime.NewMemoryStore(), identityruntime.Options{
		Resolve: func(context.Context, identityruntime.Subject, identity.Profile) (*identity.ResolvedProfile, error) {
			return nil, errors.New("test resolver must not be called")
		},
		Verify: func(context.Context, identityruntime.Subject, *identity.ResolvedProfile) (func() error, error) {
			return nil, errors.New("test verifier must not be called")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	a.Service.(*browserdiagnostics.Service).SetManager(m)
	return a
}
func identityPost(a *Handler, input IdentityInput) *httptest.ResponseRecorder {
	body, _ := json.Marshal(input)
	w := httptest.NewRecorder()
	a.Identities(w, httptest.NewRequest("POST", "/api/browser-identities", strings.NewReader(string(body))))
	return w
}
func TestIdentityAPIVersionCASAndReadOnlyGET(t *testing.T) {
	a := identityTestHandler(t)
	p := diagnosticTestProfile()
	if w := identityPost(a, IdentityInput{Action: "profile", Profile: p}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := identityPost(a, IdentityInput{Action: "profile", Profile: p}); w.Code != 409 {
		t.Fatal("stale append", w.Code, w.Body.String())
	}
	s := identityruntime.Subject{Site: "fixture", Kind: "session", Key: "business-session-1"}
	if w := identityPost(a, IdentityInput{Action: "stage", Subject: s, Reference: p.Reference()}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := identityPost(a, IdentityInput{Action: "stage", Subject: s, Reference: p.Reference()}); w.Code != 409 {
		t.Fatal("stale binding", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	a.Identities(w, httptest.NewRequest("GET", "/api/browser-identities", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"pending"`) || strings.Contains(w.Body.String(), `"verified":true`) {
		t.Fatal("GET performed verification or lost pending", w.Code, w.Body.String())
	}
	if w = identityPost(a, IdentityInput{Action: "cancel", Subject: s, Expected: 1}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = identityPost(a, IdentityInput{Action: "stage", Subject: identityruntime.Subject{Site: "fixture", Kind: "session"}, Reference: p.Reference()}); w.Code != 400 {
		t.Fatal("empty session accepted", w.Code)
	}
	if w = identityPost(a, IdentityInput{Action: "diagnose", Subject: identityruntime.Subject{Site: "other", Kind: "account", Key: "a"}, Reference: p.Reference()}); w.Code != 400 {
		t.Fatal("cross-site diagnostic accepted", w.Code)
	}
}

func TestIdentityAPIShutdownCancelsAndWaitsForInFlightOperation(t *testing.T) {
	a := identityTestHandler(t)
	op, done, ok := a.Service.Begin(context.Background())
	if !ok {
		t.Fatal("initial operation denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if a.Service.(*browserdiagnostics.Service).Shutdown(ctx) {
		t.Fatal("reported drained with an active handler")
	}
	if op.Err() == nil {
		t.Fatal("active handler was not canceled")
	}
	w := httptest.NewRecorder()
	a.Identities(w, httptest.NewRequest("GET", "/api/browser-identities", nil))
	if w.Code != 503 {
		t.Fatal("accepted operation during shutdown", w.Code)
	}
	done()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if !a.Service.(*browserdiagnostics.Service).Shutdown(ctx2) {
		t.Fatal("finished handler did not drain")
	}
}
