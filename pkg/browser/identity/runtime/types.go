// Package runtime coordinates durable browser identity bindings in one process.
// It does not own browser processes or create database tables.
package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
)

var (
	ErrNotFound     = errors.New("browser identity not found")
	ErrConflict     = errors.New("browser identity revision conflict")
	ErrCorrupt      = errors.New("invalid stored browser identity")
	ErrReferenced   = errors.New("browser identity version is referenced")
	ErrClosed       = errors.New("browser identity runtime closed")
	ErrUncertain    = errors.New("browser identity write could not be verified")
	ErrVerification = errors.New("browser identity verification required")
)

type Subject struct {
	Site string `json:"site"`
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

func (s Subject) Validate() error {
	if s.Site == "" || s.Key == "" || strings.TrimSpace(s.Site) != s.Site || strings.TrimSpace(s.Key) != s.Key || len(s.Site) > 256 || len(s.Key) > 256 || (s.Kind != "account" && s.Kind != "session") || strings.ContainsAny(s.Site+s.Key, "\x00\r\n") {
		return errors.New("identity subject requires site, account/session kind and explicit nonempty key (max 256 bytes)")
	}
	return nil
}

type Binding struct {
	Subject  Subject             `json:"subject"`
	Revision uint64              `json:"revision"`
	Active   *identity.Reference `json:"active,omitempty"`
	Pending  *identity.Reference `json:"pending,omitempty"`
	State    string              `json:"state"`
	Reason   string              `json:"reason,omitempty"`
}

func (b Binding) Clone() Binding {
	if b.Active != nil {
		r := *b.Active
		b.Active = &r
	}
	if b.Pending != nil {
		r := *b.Pending
		b.Pending = &r
	}
	return b
}
func (b Binding) Validate() error {
	if b.Subject.Validate() != nil || b.Revision == 0 {
		return ErrCorrupt
	}
	for _, r := range []*identity.Reference{b.Active, b.Pending} {
		if r != nil && (r.Site != b.Subject.Site || r.ID == "" || r.Version == 0) {
			return ErrCorrupt
		}
	}
	if (b.State == "active" && (b.Active == nil || b.Pending != nil)) || (b.State == "unbound" && (b.Active != nil || b.Pending != nil)) || ((b.State == "pending" || b.State == "failed") && b.Pending == nil) {
		return ErrCorrupt
	}
	if b.State != "active" && b.State != "unbound" && b.State != "pending" && b.State != "failed" {
		return ErrCorrupt
	}
	return nil
}

type Store interface {
	AppendProfile(context.Context, identity.Profile, uint64) error
	GetProfile(context.Context, identity.Reference) (identity.Profile, error)
	ListProfiles(context.Context, string) ([]identity.Profile, error)
	GetBinding(context.Context, Subject) (Binding, error)
	ListBindings(context.Context, string) ([]Binding, error)
	SaveBindingCAS(context.Context, Binding, uint64) error
}

type Options struct {
	Resolve func(context.Context, Subject, identity.Profile) (*identity.ResolvedProfile, error)
	// Verify persists and reads back a detached credential candidate. Its returned
	// callback publishes that candidate in memory only after the binding CAS succeeds.
	// Errors must not delete or invalidate the previous credentials.
	Verify        func(context.Context, Subject, *identity.ResolvedProfile) (func() error, error)
	VerifyTimeout time.Duration
}

type Snapshot struct {
	Binding
	InUse     int  `json:"in_use"`
	Verifying bool `json:"verifying"`
	Verified  bool `json:"verified"`
}
