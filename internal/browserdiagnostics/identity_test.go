package browserdiagnostics

import (
	"context"
	"encoding/json"
	"github.com/go-rod/rod/lib/proto"
	"net/http"
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
	find := func(checks []IdentityCheck) IdentityCheck {
		for _, c := range checks {
			if c.Field == "加载后高熵 Client Hints" {
				return c
			}
		}
		t.Fatal("missing high-entropy check")
		return IdentityCheck{}
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
