package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Resolve reads the installed Chromium's native identity. The secure document is
// fulfilled entirely in CDP; it cannot make a network request or use credentials.
func Resolve(ctx context.Context, browser *rod.Browser, declaration Profile) (*ResolvedProfile, error) {
	p := declaration.Clone()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if browser == nil {
		return nil, fmt.Errorf("identity browser required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b := browser.Context(ctx)
	v, err := proto.BrowserGetVersion{}.Call(b)
	if err != nil {
		return nil, fmt.Errorf("%w: read browser version: %v", ErrIncompatible, err)
	}
	if !strings.HasPrefix(v.Product, "Chrome/") && !strings.HasPrefix(v.Product, "HeadlessChrome/") {
		return nil, fmt.Errorf("%w: native Chromium required", ErrIncompatible)
	}
	// An isolated context prevents even an explicit user-data-dir from exposing
	// its cookies or storage to the synthetic probe.
	incognito, err := b.Incognito()
	if err != nil {
		return nil, err
	}
	defer func() { _ = incognito.Context(context.WithoutCancel(ctx)).Timeout(3 * time.Second).Close() }()
	page, err := incognito.NoDefaultDevice().Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, err
	}
	const origin = "https://papa-identity.invalid/__native__"
	if err = (proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{{URLPattern: "*", RequestStage: proto.FetchRequestStageRequest}}}).Call(page); err != nil {
		return nil, err
	}
	wait := page.EachEvent(func(e *proto.FetchRequestPaused) {
		if e.Request.URL != origin {
			_ = (proto.FetchFailRequest{RequestID: e.RequestID, ErrorReason: proto.NetworkErrorReasonBlockedByClient}).Call(page)
			return
		}
		_ = (proto.FetchFulfillRequest{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*proto.FetchHeaderEntry{{Name: "Content-Type", Value: "text/html"}, {Name: "Content-Security-Policy", Value: "default-src 'none'"}}, Body: []byte("<!doctype html><title>native identity</title>")}).Call(page)
	})
	go wait()
	if err = page.Navigate(origin); err != nil {
		return nil, fmt.Errorf("%w: native secure probe: %v", ErrIncompatible, err)
	}
	if err = page.WaitLoad(); err != nil {
		return nil, err
	}
	result, err := page.Eval(`async () => {
 if(!navigator.userAgentData) throw new Error('native User-Agent Client Hints unavailable');
 const u=navigator.userAgentData;
 const h=await u.getHighEntropyValues(['architecture','bitness','model','platformVersion','fullVersionList','wow64']);
 return {platform:navigator.platform,metadata:{brands:u.brands,mobile:u.mobile,platform:u.platform,...h}};
}`)
	if err != nil {
		return nil, fmt.Errorf("%w: native metadata unavailable: %v", ErrIncompatible, err)
	}
	var native struct {
		Platform string                            `json:"platform"`
		Metadata *proto.EmulationUserAgentMetadata `json:"metadata"`
	}
	if err = result.Value.Unmarshal(&native); err != nil {
		return nil, err
	}
	r := &ResolvedProfile{Reference: p.Reference(), Profile: p, Runtime: RuntimeIdentity{Product: v.Product, Revision: v.Revision, UserAgent: v.UserAgent, JSVersion: v.JsVersion, Platform: native.Platform}, UserAgent: v.UserAgent, AcceptLanguage: strings.Join(p.Languages, ","), Platform: native.Platform, UserAgentMetadata: native.Metadata}
	r.nativeSeal = r.fingerprint()
	if err = r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// CheckRuntime refuses to reinterpret a frozen identity after a browser update.
func CheckRuntime(ctx context.Context, browser *rod.Browser, r *ResolvedProfile) error {
	if err := r.validateNative(); err != nil {
		return err
	}
	if browser == nil {
		return fmt.Errorf("identity browser required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := proto.BrowserGetVersion{}.Call(browser.Context(ctx))
	if err != nil {
		return fmt.Errorf("%w: read runtime: %v", ErrIncompatible, err)
	}
	if v.Product != r.Runtime.Product || v.Revision != r.Runtime.Revision || v.UserAgent != r.Runtime.UserAgent || v.JsVersion != r.Runtime.JSVersion {
		return fmt.Errorf("%w: native browser runtime changed", ErrIncompatible)
	}
	return nil
}

// Apply configures the current blank document and future documents before the
// first navigation. Its only optional JavaScript adapter changes webdriver.
func Apply(page *rod.Page, resolved *ResolvedProfile) (AdapterObservation, error) {
	if page == nil {
		return AdapterObservation{}, fmt.Errorf("identity page required")
	}
	ctx, cancel := context.WithTimeout(page.GetContext(), 10*time.Second)
	defer cancel()
	page = page.Context(ctx)
	r := resolved.Clone()
	if err := r.validateNative(); err != nil {
		return AdapterObservation{}, err
	}
	p := r.Profile
	steps := []struct {
		name string
		run  func() error
	}{
		{"user agent", func() error {
			return (proto.NetworkSetUserAgentOverride{UserAgent: r.UserAgent, AcceptLanguage: r.AcceptLanguage, Platform: r.Platform, UserAgentMetadata: cloneMetadata(r.UserAgentMetadata)}).Call(page)
		}},
		{"locale", func() error { return (proto.EmulationSetLocaleOverride{Locale: p.Locale}).Call(page) }},
		{"timezone", func() error { return (proto.EmulationSetTimezoneOverride{TimezoneID: p.Timezone}).Call(page) }},
		{"metrics", func() error {
			return (proto.EmulationSetDeviceMetricsOverride{Width: p.Viewport.Width, Height: p.Viewport.Height, ScreenWidth: &p.Screen.Width, ScreenHeight: &p.Screen.Height, DeviceScaleFactor: p.DeviceScaleFactor, Mobile: r.UserAgentMetadata.Mobile}).Call(page)
		}},
		{"touch", func() error {
			return (proto.EmulationSetTouchEmulationEnabled{Enabled: p.Touch, MaxTouchPoints: func() *int {
				if !p.Touch {
					return nil
				}
				v := p.MaxTouchPoints
				return &v
			}()}).Call(page)
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			return AdapterObservation{}, &InitializationError{Stage: step.name, Cause: fmt.Errorf("%w: %v", ErrIncompatible, err)}
		}
	}
	a := AdapterObservation{ID: p.Adapter.ID, Version: p.Adapter.Version, Enabled: p.Adapter.Enabled}
	if !p.Adapter.Enabled {
		return a, nil
	}
	_, err := page.EvalOnNewDocument(webdriverAdapter)
	if err != nil {
		return a, &InitializationError{Stage: "adapter registration", Cause: fmt.Errorf("%w: %v", ErrIncompatible, err)}
	}
	a.Registered = true
	return a, nil
}

// A fixed, narrowly scoped adapter: never replaces UA, languages, hardware,
// fonts, canvas or WebGL. No external dependency or Node runtime is used.
const webdriverAdapter = `(() => {
 const getter=function papaIdentityWebdriverV1(){return false};
 Object.defineProperty(Navigator.prototype,'webdriver',{get:getter,configurable:true,enumerable:true});
})();`
