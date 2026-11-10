package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-rod/rod/lib/proto"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

func diagnosticTestProfile() identity.Profile {
	return identity.Profile{Site: "fixture", ID: "desktop", Version: 1, SchemaVersion: 1, UAPolicy: identity.NativeChromium, Languages: []string{"en-US", "en"}, Locale: "en-US", Timezone: "UTC", Viewport: identity.Size{Width: 1280, Height: 720}, Screen: identity.Size{Width: 1920, Height: 1080}, DeviceScaleFactor: 1, Adapter: identity.AdapterConfig{ID: "webdriver", Version: "1", Enabled: true}}
}
func identityTestApp(t *testing.T) *App {
	t.Helper()
	a, _ := runtimeTestApp(t)
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
	a.identityState.manager = m
	return a
}
func identityPost(a *App, input identityInput) *httptest.ResponseRecorder {
	body, _ := json.Marshal(input)
	w := httptest.NewRecorder()
	a.browserIdentitiesAPI(w, httptest.NewRequest("POST", "/api/browser-identities", strings.NewReader(string(body))))
	return w
}
func TestIdentityAPIVersionCASAndReadOnlyGET(t *testing.T) {
	a := identityTestApp(t)
	p := diagnosticTestProfile()
	if w := identityPost(a, identityInput{Action: "profile", Profile: p}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := identityPost(a, identityInput{Action: "profile", Profile: p}); w.Code != 409 {
		t.Fatal("stale append", w.Code, w.Body.String())
	}
	s := identityruntime.Subject{Site: "fixture", Kind: "session", Key: "business-session-1"}
	if w := identityPost(a, identityInput{Action: "stage", Subject: s, Reference: p.Reference()}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := identityPost(a, identityInput{Action: "stage", Subject: s, Reference: p.Reference()}); w.Code != 409 {
		t.Fatal("stale binding", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	a.browserIdentitiesAPI(w, httptest.NewRequest("GET", "/api/browser-identities", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"pending"`) || strings.Contains(w.Body.String(), `"verified":true`) {
		t.Fatal("GET performed verification or lost pending", w.Code, w.Body.String())
	}
	if w = identityPost(a, identityInput{Action: "cancel", Subject: s, Expected: 1}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = identityPost(a, identityInput{Action: "stage", Subject: identityruntime.Subject{Site: "fixture", Kind: "session"}, Reference: p.Reference()}); w.Code != 400 {
		t.Fatal("empty session accepted", w.Code)
	}
	if w = identityPost(a, identityInput{Action: "diagnose", Subject: identityruntime.Subject{Site: "other", Kind: "account", Key: "a"}, Reference: p.Reference()}); w.Code != 400 {
		t.Fatal("cross-site diagnostic accepted", w.Code)
	}
}

func TestIdentityAPIShutdownCancelsAndWaitsForInFlightOperation(t *testing.T) {
	a := identityTestApp(t)
	a.identityState.ctx, a.identityState.cancel = context.WithCancel(context.Background())
	op, done, ok := a.beginIdentityOperation(context.Background())
	if !ok {
		t.Fatal("initial operation denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if a.stopBrowserIdentities(ctx) {
		t.Fatal("reported drained with an active handler")
	}
	if op.Err() == nil {
		t.Fatal("active handler was not canceled")
	}
	w := httptest.NewRecorder()
	a.browserIdentitiesAPI(w, httptest.NewRequest("GET", "/api/browser-identities", nil))
	if w.Code != 503 {
		t.Fatal("accepted operation during shutdown", w.Code)
	}
	done()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if !a.stopBrowserIdentities(ctx2) {
		t.Fatal("finished handler did not drain")
	}
}

func TestIdentityFixtureRejectsOtherPathsAndEchoesNoCredentials(t *testing.T) {
	f, err := newIdentityFixture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	r, _ := http.NewRequest("GET", f.URL, nil)
	r.Header.Set("User-Agent", "fixture-UA")
	r.Header.Set("Authorization", "secret-token")
	r.Header.Set("Cookie", "session=secret-cookie")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	bytes, _ := json.Marshal(f.firstHeaders())
	if strings.Contains(string(bytes), "secret") || !strings.Contains(string(bytes), "fixture-UA") {
		t.Fatal(string(bytes))
	}
	for _, suffix := range []string{"/else", "?redirect=https://example.test"} {
		resp, err = http.Get(f.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatal("unscoped path", resp.StatusCode)
		}
	}
}

func TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	r := runIdentityDiagnosticWithConfig(ctx, diagnosticTestProfile(), identityruntime.Subject{Site: "fixture", Kind: "session", Key: "anonymous"}, browser.PoolConfig{BrowserPath: bin, Headless: true, NoSandbox: true}, nil)
	if !r.OK {
		b, _ := json.MarshalIndent(r, "", "  ")
		t.Fatal(string(b))
	}
	if r.FirstScript == nil || r.Observation == nil || !r.Observation.Adapter.Registered || !r.Observation.Adapter.Verified {
		t.Fatal("missing first-document/adapter evidence")
	}
	if r.FirstHeaders["Cookie"] != "" || r.FirstHeaders["Authorization"] != "" {
		t.Fatal("credentials in diagnostic report")
	}
}

func TestIdentityDiagnosticDoesNotPassMissingOrMismatchedHighEntropy(t *testing.T) {
	p := diagnosticTestProfile()
	metadata := &proto.EmulationUserAgentMetadata{Brands: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "157"}}, FullVersionList: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "157.0.1.2"}}, Platform: "Windows", PlatformVersion: "19.0.0", Architecture: "x86", Bitness: "64"}
	r := &identity.ResolvedProfile{Profile: p, UserAgentMetadata: metadata}
	o := &identity.Observation{ClientHintsAvailable: true, UserAgentMetadata: metadata}
	find := func(checks []identityCheck) identityCheck {
		for _, c := range checks {
			if c.Field == "加载后高熵 Client Hints" {
				return c
			}
		}
		t.Fatal("missing high-entropy check")
		return identityCheck{}
	}
	if c := find(compareIdentityDiagnostic(r, nil, o, o)); c.Status != "passed" {
		t.Fatal(c)
	}
	o.ClientHintsReason = "high entropy unavailable"
	if c := find(compareIdentityDiagnostic(r, nil, o, o)); c.Status != "unavailable" {
		t.Fatal(c)
	}
	o.ClientHintsReason = ""
	changed := *metadata
	changed.Architecture = "arm"
	o.UserAgentMetadata = &changed
	if c := find(compareIdentityDiagnostic(r, nil, o, o)); c.Status != "failed" {
		t.Fatal(c)
	}
	if !headerCHBrandsMatch(`"Not/A)Brand";v="99", "Chromium";v="157"`, metadata.Brands) {
		t.Fatal("GREASE should not change result")
	}
}
