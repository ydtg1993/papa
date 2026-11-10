package identity

import (
	"context"
	"errors"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
	"math"
	"testing"
)

func fixtureProfile() Profile {
	return Profile{Site: "fixture", ID: "desktop", Version: 1, SchemaVersion: 1, UAPolicy: NativeChromium, Languages: []string{"en-US", "en"}, Locale: "en-US", Timezone: "UTC", Viewport: Size{1280, 720}, Screen: Size{1920, 1080}, DeviceScaleFactor: 1}
}

type runtimeVersionClient struct{}

func (runtimeVersionClient) Event() <-chan *cdp.Event { return nil }
func (runtimeVersionClient) Call(context.Context, string, string, interface{}) ([]byte, error) {
	return []byte(`{"product":"Chrome/200.0.0.0","revision":"new","userAgent":"native","jsVersion":"new"}`), nil
}
func TestIdentityNativeRuntimeMismatch(t *testing.T) {
	p := fixtureProfile()
	r := &ResolvedProfile{Reference: p.Reference(), Profile: p, Runtime: RuntimeIdentity{Product: "Chrome/199.0.0.0", Revision: "old", UserAgent: "native", JSVersion: "old", Platform: "Win32"}, UserAgent: "native", Platform: "Win32", AcceptLanguage: "en-US,en", UserAgentMetadata: &proto.EmulationUserAgentMetadata{Brands: []*proto.EmulationUserAgentBrandVersion{{Brand: "Chromium", Version: "199"}}}}
	r.nativeSeal = r.fingerprint()
	if err := CheckRuntime(context.Background(), rod.New().Client(runtimeVersionClient{}), r); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("updated runtime accepted: %v", err)
	}
	c := r.Clone()
	c.UserAgentMetadata.Brands[0].Version = "200"
	if r.UserAgentMetadata.Brands[0].Version != "199" {
		t.Fatal("metadata alias")
	}
	if c.Validate() == nil {
		t.Fatal("mutated native metadata accepted")
	}
}

func TestIdentityProfileValidation(t *testing.T) {
	p := fixtureProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Profile){"missing reference": func(p *Profile) { p.Version = 0 }, "foreign UA": func(p *Profile) { p.UAPolicy = "firefox" }, "locale drift": func(p *Profile) { p.Locale = "zh-CN" }, "header injection": func(p *Profile) { p.Languages = []string{"en-US\r\nX:1"} }, "nan": func(p *Profile) { p.DeviceScaleFactor = math.NaN() }, "infinite": func(p *Profile) { p.DeviceScaleFactor = math.Inf(1) }, "screen": func(p *Profile) { p.Screen.Width = 400 }, "touch": func(p *Profile) { p.MaxTouchPoints = 2 }, "timezone": func(p *Profile) { p.Timezone = "Invalid/Zone" }, "adapter": func(p *Profile) { p.Adapter = AdapterConfig{Enabled: true, ID: "whole-stealth", Version: "latest"} }} {
		t.Run(name, func(t *testing.T) {
			p := fixtureProfile()
			change(&p)
			if p.Validate() == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}
func TestIdentityProfileCloneAndResolvedMutation(t *testing.T) {
	p := fixtureProfile()
	c := p.Clone()
	c.Languages[0] = "fr-FR"
	if p.Languages[0] != "en-US" {
		t.Fatal("declaration alias")
	}
	r := &ResolvedProfile{Profile: p, Reference: p.Reference()}
	r.nativeSeal = r.fingerprint()
	c2 := r.Clone()
	if c2.nativeSeal != r.nativeSeal {
		t.Fatal("clone lost native seal")
	}
	c2.Profile.Languages[0] = "fr-FR"
	if c2.fingerprint() == r.nativeSeal {
		t.Fatal("mutation not detected")
	}
}
