package browser

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
)

type identityPageKey struct {
	Target  proto.TargetTargetID
	Session proto.TargetSessionID
}
type identityPageState struct {
	mu       sync.Mutex
	snapshot *identity.ResolvedProfile
	adapter  identity.AdapterObservation
	headers  map[string]string
}

// Keys contain no Rod/page pointers. A browser-level event subscription removes
// the entry on close/disconnect, independent of request context cancellation.
var identityPages sync.Map

func pageIdentityKey(p *rod.Page) identityPageKey { return identityPageKey{p.TargetID, p.SessionID} }
func trackPageIdentity(page *rod.Page, state *identityPageState) {
	key := pageIdentityKey(page)
	wait := page.Browser().Context(context.Background()).EachEvent(
		func(e *proto.TargetDetachedFromTarget) bool { return e.SessionID == key.Session },
		func(e *proto.TargetTargetDestroyed) bool { return e.TargetID == key.Target },
	)
	identityPages.Store(key, state)
	go func() { defer identityPages.Delete(key); wait() }()
}
func lookupPageIdentity(p *rod.Page) *identityPageState {
	if p == nil {
		return nil
	}
	v, ok := identityPages.Load(pageIdentityKey(p))
	if !ok {
		return nil
	}
	return v.(*identityPageState)
}

// ResolveIdentity freezes a profile against the actual borrowed browser. It
// does not read account credentials or publish a binding.
func (b *Browser) ResolveIdentity(ctx context.Context, p identity.Profile) (*identity.ResolvedProfile, error) {
	if b == nil || b.Browser == nil {
		return nil, fmt.Errorf("identity browser required")
	}
	if err := b.validateIdentityLaunch(); err != nil {
		return nil, err
	}
	return identity.Resolve(ctx, b.Browser, p)
}

func (b *Browser) validateIdentityLaunch() error {
	if b.pool == nil {
		return nil
	}
	for key := range b.pool.cfg.Flags {
		switch strings.ToLower(strings.TrimLeft(key, "-")) {
		case "user-agent", "lang", "accept-lang":
			return fmt.Errorf("%w: browser.flags.%s overrides managed identity", identity.ErrConflict, key)
		}
	}
	return nil
}

func protectedIdentityHeader(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	return k == "user-agent" || k == "accept-language" || strings.HasPrefix(k, "sec-ch-ua")
}

func validateIdentityHeaders(r *identity.ResolvedProfile, headers map[string]string, remove []string, source string) error {
	for key, value := range headers {
		if !protectedIdentityHeader(key) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "user-agent":
			if value == r.UserAgent {
				continue
			}
		case "accept-language":
			if strings.EqualFold(strings.ReplaceAll(value, " ", ""), r.AcceptLanguage) {
				continue
			}
		}
		return fmt.Errorf("%w: %s.%s overrides managed identity", identity.ErrConflict, source, key)
	}
	for _, key := range remove {
		if protectedIdentityHeader(key) {
			return fmt.Errorf("%w: %s.RemoveHeaders.%s removes managed identity", identity.ErrConflict, source, key)
		}
	}
	return nil
}

// ValidateIdentityOptions checks legacy overrides before any navigation.
func ValidateIdentityOptions(r *identity.ResolvedProfile, headers map[string]string, remove []string, device *devices.Device) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if device != nil {
		return fmt.Errorf("%w: PageOptions.Device overrides managed identity", identity.ErrConflict)
	}
	return validateIdentityHeaders(r, headers, remove, "PageOptions.Headers")
}

func ordinaryIdentityHeaders(headers map[string]string) map[string]string {
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		if !protectedIdentityHeader(k) {
			clean[k] = v
		}
	}
	return clean
}

func (b *Browser) initializePageIdentity(ctx context.Context, page *rod.Page, opts PageOptions, defaults map[string]string) (*identityPageState, error) {
	if opts.Identity == nil {
		return nil, nil
	}
	r := opts.Identity.Clone()
	if err := ValidateIdentityOptions(r, opts.Headers, opts.RemoveHeaders, opts.Device); err != nil {
		return nil, err
	}
	if b.defaultDevice != nil {
		return nil, fmt.Errorf("%w: pool DefaultDevice overrides managed identity", identity.ErrConflict)
	}
	if err := validateIdentityHeaders(r, defaults, nil, "pool.DefaultHeaders"); err != nil {
		return nil, err
	}
	if err := b.validateIdentityLaunch(); err != nil {
		return nil, err
	}
	if err := identity.CheckRuntime(ctx, b.Browser, r); err != nil {
		return nil, err
	}
	a, err := identity.Apply(page, r)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	args := mergeExtraHeaders(ordinaryIdentityHeaders(defaults), ordinaryIdentityHeaders(opts.Headers), opts.RemoveHeaders)
	for i := 0; i < len(args); i += 2 {
		headers[args[i]] = args[i+1]
	}
	state := &identityPageState{snapshot: r, adapter: a, headers: headers}
	trackPageIdentity(page, state)
	return state, nil
}

// ManagedIdentity returns a detached snapshot, never the page's stored pointer.
func ManagedIdentity(page *rod.Page) *identity.ResolvedProfile {
	s := lookupPageIdentity(page)
	if s == nil {
		return nil
	}
	return s.snapshot.Clone()
}

// ObserveIdentity reports actual values and separately tracked registration and
// verified execution evidence. It never reads cookies or origin storage.
func ObserveIdentity(page *rod.Page) (*identity.Observation, error) {
	s := lookupPageIdentity(page)
	if s == nil {
		return nil, fmt.Errorf("page has no managed identity")
	}
	o, err := identity.Observe(page, s.snapshot)
	if err != nil {
		return nil, err
	}
	o.Adapter.Registered = s.adapter.Registered
	return o, nil
}

// configureIdentityPage returns handled=true for identity pages. Ordinary
// headers are merged with existing values so authorization is not discarded.
func configureIdentityPage(ctx context.Context, page *rod.Page, opts PageOptions) (bool, error) {
	s := lookupPageIdentity(page)
	if s == nil {
		if opts.Identity != nil {
			return false, fmt.Errorf("%w: identity must be assigned when creating a page", identity.ErrConflict)
		}
		return false, nil
	}
	if opts.Identity != nil && !reflect.DeepEqual(opts.Identity, s.snapshot) {
		return true, fmt.Errorf("%w: cannot change an existing page identity", identity.ErrConflict)
	}
	if err := ValidateIdentityOptions(s.snapshot, opts.Headers, opts.RemoveHeaders, opts.Device); err != nil {
		return true, err
	}
	if err := identity.CheckRuntime(ctx, page.Browser(), s.snapshot); err != nil {
		return true, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Headers != nil {
		args := mergeExtraHeaders(s.headers, ordinaryIdentityHeaders(opts.Headers), nil)
		if _, err := page.SetExtraHeaders(args); err != nil {
			return true, err
		}
		next := map[string]string{}
		for i := 0; i < len(args); i += 2 {
			next[args[i]] = args[i+1]
		}
		s.headers = next
	}
	return true, nil
}

// DiagnosticConfig creates a credential-free reproduction configuration.
// Only explicitly safe environment flags survive; omissions must be displayed
// as unverified conditions. No production egress grants are copied.
func (p *Pool) DiagnosticConfig() (PoolConfig, []string) {
	if p == nil {
		return PoolConfig{}, []string{"browser pool unavailable"}
	}
	c := p.cfg
	out := PoolConfig{Size: 1, Headless: c.Headless, NoSandbox: c.NoSandbox, Leakless: c.Leakless, BinarySource: c.BinarySource, BrowserPath: c.BrowserPath, Flags: map[string]string{}}
	// Protected identity declarations must still reach the production initializer
	// so a conflicting legacy setting cannot turn into a diagnostic false pass.
	out.DefaultHeaders = map[string]string{}
	for k, v := range p.headersSnapshot() {
		if protectedIdentityHeader(k) {
			out.DefaultHeaders[k] = v
		}
	}
	if c.DefaultDevice != nil {
		device := *c.DefaultDevice
		device.Capabilities = append([]string(nil), device.Capabilities...)
		out.DefaultDevice = &device
	}
	allowed := map[string]bool{"headless": true, "no-sandbox": true, "disable-gpu": true, "use-gl": true, "use-angle": true, "disable-software-rasterizer": true, "force-color-profile": true, "hide-scrollbars": true, "disable-dev-shm-usage": true}
	omissions := []string{}
	for k, v := range c.Flags {
		if allowed[strings.TrimLeft(k, "-")] {
			out.Flags[k] = v
		} else if key := strings.ToLower(strings.TrimLeft(k, "-")); key == "user-agent" || key == "lang" || key == "accept-lang" {
			// The validator rejects these keys independent of their value. Retain
			// the conflict without reproducing a potentially sensitive argument.
			out.Flags[k] = ""
			omissions = append(omissions, "identity conflict: browser flag "+key)
		} else {
			omissions = append(omissions, "browser flag omitted: "+strings.TrimLeft(k, "-"))
		}
	}
	if c.ProxyManager != nil {
		omissions = append(omissions, "proxy and network reputation not reproduced")
	}
	if c.DefaultDevice != nil {
		omissions = append(omissions, "legacy default device is incompatible with managed identity")
	}
	if len(p.headersSnapshot()) > 0 || len(c.DefaultCookies) > 0 {
		omissions = append(omissions, "credentials and ordinary default headers excluded")
	}
	omissions = append(omissions, "target authentication and HTTP transport fingerprint not tested")
	sort.Strings(omissions)
	return out, omissions
}
