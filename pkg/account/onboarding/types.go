// Package onboarding implements provider-declared account login workflows.
// Site protocols belong to the caller. Neither profiles nor public progress
// contain credentials; secrets require an explicitly supplied encrypted Store.
package onboarding

import (
	"context"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
)

var (
	ErrConflict      = errors.New("account onboarding revision conflict")
	ErrDenied        = errors.New("account onboarding operator denied")
	ErrGone          = errors.New("login process ended or was terminated by restart")
	ErrBusy          = errors.New("account login capacity exhausted")
	ErrIdentity      = errors.New("verified account identity does not match")
	ErrNotConfigured = errors.New("account onboarding requires providers and encrypted secret storage")
)

type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Persist permits the operator to opt in to encrypted storage (e.g. a
	// password). OTPs should never set Persist.
	Persist bool `json:"persist,omitempty"`
}
type Method struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Kind   string  `json:"kind"` // password, input, qr, import, remote
	Fields []Field `json:"fields"`
}
type Descriptor struct {
	Site    string   `json:"site"`
	Label   string   `json:"label"`
	Methods []Method `json:"methods"`
}
type Identity struct {
	Subject string   `json:"subject"`
	Display string   `json:"display"`
	Scopes  []string `json:"scopes"`
	HTTP    bool     `json:"http"`
	Browser bool     `json:"browser"`
}
type Profile struct {
	Key             account.Key `json:"key"`
	Revision        uint64      `json:"revision"`
	Label           string      `json:"label"`
	ExpectedSubject string      `json:"expected_subject,omitempty"`
	Source          string      `json:"source"`
	State           string      `json:"state"` // draft or ready
	Identity        *Identity   `json:"identity,omitempty"`
	VerifiedAt      time.Time   `json:"verified_at,omitempty"`
	UpdatedAt       time.Time   `json:"updated_at"`
	// Secret references are opaque random names, never returned by OA.
	SecretRef string `json:"-"`
	InputsRef string `json:"-"`
}

// Provider is registered by the business module. Descriptors and progress
// labels are public: never include tokens, login URLs or raw provider errors.
// Config supplies runtime authentication callbacks and exact origin/proxy/UA
// binding. It must not perform login. SavedInputs returns only opted-in fields.
// Start, Next and Verify must honor context cancellation. Their context carries
// the site's request governance and does not hold a crawl task execution slot.
type Provider interface {
	Descriptor() Descriptor
	Config(context.Context, Profile, func(context.Context) (map[string]string, error)) (account.Config, error)
	Start(context.Context, Profile, string, map[string]string) (Flow, error)
	Verify(context.Context, Profile, *account.Credentials) (Identity, error)
}
type Flow interface {
	Next(context.Context, map[string]string) (Step, error)
	Close() error
}
type Step struct {
	State        string // waiting_input, waiting_scan, remote, candidate
	Fields       []Field
	ImageDataURL string // data:image/png;base64,... only
	Candidate    *account.Credentials
	Remote       *Remote
}
type Progress struct {
	ID           string      `json:"id"`
	Key          account.Key `json:"key"`
	Revision     uint64      `json:"revision"`
	State        string      `json:"state"`
	ExpiresAt    time.Time   `json:"expires_at"`
	Fields       []Field     `json:"fields,omitempty"`
	ImageDataURL string      `json:"image_data_url,omitempty"`
	Identity     *Identity   `json:"identity,omitempty"`
	Failure      string      `json:"failure,omitempty"`
	Remote       bool        `json:"remote"`
}
type Store interface {
	List(context.Context) ([]Profile, error)
	Get(context.Context, account.Key) (Profile, error)
	SaveCAS(context.Context, Profile, uint64) error
}
type Options struct {
	Manager  *account.Manager
	Profiles Store
	// Secrets must provide authenticated encryption, e.g. account.FileStore.
	// No default key and no plaintext fallback exist.
	Secrets     account.Store
	MaxActive   int
	MaxRetained int // total public process records, including active processes
	TTL         time.Duration
	StepTimeout time.Duration
	Retention   time.Duration
	Context     func(context.Context, string) context.Context
}
