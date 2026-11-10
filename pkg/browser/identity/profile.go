// Package identity defines versioned browser declarations and native Chromium
// application. It contains no persistence or account credentials.
package identity

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"golang.org/x/text/language"
)

const SchemaVersion = 1
const CheckerVersion = "papa-identity-v1"
const NativeChromium = "native-chromium"

var ErrIncompatible = errors.New("browser identity incompatible")
var ErrConflict = errors.New("browser identity conflict")

// InitializationError identifies the failing managed initialization step.
// Retrying an incompatible frozen identity cannot repair its configuration.
type InitializationError struct {
	Stage string
	Cause error
}

func (e *InitializationError) Error() string {
	return fmt.Sprintf("browser identity initialization (%s): %v", e.Stage, e.Cause)
}
func (e *InitializationError) Unwrap() error   { return e.Cause }
func (e *InitializationError) Retryable() bool { return false }

type Reference struct {
	Site    string `json:"site"`
	ID      string `json:"id"`
	Version uint64 `json:"version"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (r Reference) Validate() error {
	if !identifier.MatchString(r.Site) || !identifier.MatchString(r.ID) || r.Version == 0 {
		return fmt.Errorf("identity reference requires valid site/id and positive version")
	}
	return nil
}

type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
type AdapterConfig struct {
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	Enabled bool   `json:"enabled"`
}

// Profile is an immutable revision declaration. Callers may edit their own
// value; every runtime boundary takes a deep copy.
type Profile struct {
	Site              string        `json:"site"`
	ID                string        `json:"id"`
	Version           uint64        `json:"version"`
	SchemaVersion     int           `json:"schema_version"`
	UAPolicy          string        `json:"ua_policy"`
	Languages         []string      `json:"languages"`
	Locale            string        `json:"locale"`
	Timezone          string        `json:"timezone"`
	Viewport          Size          `json:"viewport"`
	Screen            Size          `json:"screen"`
	DeviceScaleFactor float64       `json:"device_scale_factor"`
	Touch             bool          `json:"touch"`
	MaxTouchPoints    int           `json:"max_touch_points"`
	Adapter           AdapterConfig `json:"adapter"`
}

func (p Profile) Reference() Reference { return Reference{p.Site, p.ID, p.Version} }
func (p Profile) Clone() Profile       { p.Languages = append([]string(nil), p.Languages...); return p }
func (p Profile) Validate() error {
	if err := p.Reference().Validate(); err != nil {
		return err
	}
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported identity schema_version %d", p.SchemaVersion)
	}
	if p.UAPolicy != NativeChromium {
		return fmt.Errorf("identity ua_policy must be %q", NativeChromium)
	}
	if len(p.Languages) == 0 || len(p.Languages) > 8 {
		return fmt.Errorf("identity languages requires 1..8 values")
	}
	seen := map[string]bool{}
	for _, v := range append(append([]string(nil), p.Languages...), p.Locale) {
		if len(v) > 64 || strings.TrimSpace(v) != v || strings.ContainsAny(v, ",;\r\n") {
			return fmt.Errorf("invalid identity language or locale")
		}
		if _, err := language.Parse(v); err != nil || v == "" {
			return fmt.Errorf("invalid identity language or locale")
		}
	}
	for _, v := range p.Languages {
		if seen[strings.ToLower(v)] {
			return fmt.Errorf("duplicate identity language")
		}
		seen[strings.ToLower(v)] = true
	}
	if !strings.EqualFold(p.Locale, p.Languages[0]) {
		return fmt.Errorf("identity locale must equal first language")
	}
	if p.Timezone == "" || len(p.Timezone) > 128 {
		return fmt.Errorf("identity timezone required")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("invalid identity timezone: %w", err)
	}
	for _, s := range []Size{p.Viewport, p.Screen} {
		if s.Width < 320 || s.Height < 200 || s.Width > 8192 || s.Height > 8192 {
			return fmt.Errorf("identity dimensions out of bounds")
		}
	}
	if p.Viewport.Width > p.Screen.Width || p.Viewport.Height > p.Screen.Height {
		return fmt.Errorf("identity viewport exceeds screen")
	}
	if math.IsNaN(p.DeviceScaleFactor) || math.IsInf(p.DeviceScaleFactor, 0) || p.DeviceScaleFactor < 0.5 || p.DeviceScaleFactor > 4 {
		return fmt.Errorf("identity device_scale_factor out of bounds")
	}
	if (!p.Touch && p.MaxTouchPoints != 0) || (p.Touch && (p.MaxTouchPoints < 1 || p.MaxTouchPoints > 16)) {
		return fmt.Errorf("identity touch/max_touch_points inconsistent")
	}
	if p.Adapter.Enabled && (p.Adapter.ID != "webdriver" || p.Adapter.Version != "1") {
		return fmt.Errorf("unsupported identity adapter")
	}
	if !p.Adapter.Enabled && (p.Adapter.ID != "" || p.Adapter.Version != "") && (p.Adapter.ID != "webdriver" || p.Adapter.Version != "1") {
		return fmt.Errorf("unsupported identity adapter")
	}
	return nil
}

type RuntimeIdentity struct {
	Product   string `json:"product"`
	Revision  string `json:"revision"`
	UserAgent string `json:"user_agent"`
	JSVersion string `json:"js_version"`
	Platform  string `json:"platform"`
}

type ResolvedProfile struct {
	nativeSeal        [32]byte
	Reference         Reference                         `json:"reference"`
	Profile           Profile                           `json:"profile"`
	Runtime           RuntimeIdentity                   `json:"runtime"`
	UserAgent         string                            `json:"user_agent"`
	AcceptLanguage    string                            `json:"accept_language"`
	Platform          string                            `json:"platform"`
	UserAgentMetadata *proto.EmulationUserAgentMetadata `json:"user_agent_metadata"`
}

func cloneMetadata(m *proto.EmulationUserAgentMetadata) *proto.EmulationUserAgentMetadata {
	if m == nil {
		return nil
	}
	c := *m
	clone := func(src []*proto.EmulationUserAgentBrandVersion) []*proto.EmulationUserAgentBrandVersion {
		out := make([]*proto.EmulationUserAgentBrandVersion, len(src))
		for i, v := range src {
			if v != nil {
				v2 := *v
				out[i] = &v2
			}
		}
		return out
	}
	c.Brands = clone(m.Brands)
	c.FullVersionList = clone(m.FullVersionList)
	return &c
}
func (r *ResolvedProfile) Clone() *ResolvedProfile {
	if r == nil {
		return nil
	}
	c := *r
	c.Profile = r.Profile.Clone()
	c.UserAgentMetadata = cloneMetadata(r.UserAgentMetadata)
	return &c
}
func (r *ResolvedProfile) Validate() error {
	if r == nil {
		return fmt.Errorf("identity resolved profile required")
	}
	if err := r.Profile.Validate(); err != nil {
		return err
	}
	if r.Reference != r.Profile.Reference() || r.UserAgent == "" || r.UserAgent != r.Runtime.UserAgent || r.Platform == "" || r.Platform != r.Runtime.Platform || r.AcceptLanguage != strings.Join(r.Profile.Languages, ",") || r.UserAgentMetadata == nil || len(r.UserAgentMetadata.Brands) == 0 {
		return fmt.Errorf("%w: malformed resolved snapshot", ErrIncompatible)
	}
	if r.nativeSeal != ([32]byte{}) && r.nativeSeal != r.fingerprint() {
		return fmt.Errorf("%w: resolved snapshot was modified", ErrIncompatible)
	}
	return nil
}

func (r *ResolvedProfile) fingerprint() [32]byte {
	data, _ := json.Marshal(r)
	return sha256.Sum256(data)
}
func (r *ResolvedProfile) validateNative() error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.nativeSeal == ([32]byte{}) {
		return fmt.Errorf("%w: snapshot must be resolved against native Chromium", ErrIncompatible)
	}
	return nil
}
