// Package account coordinates explicitly registered crawl accounts in one process.
package account

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account/contract"
	"net/http"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

var (
	ErrUnknown     = errors.New("account is not registered")
	ErrPaused      = errors.New("account is paused")
	ErrExpired     = errors.New("account credentials expired")
	ErrNeedsVerify = errors.New("account credentials require verification")
	ErrBudget      = errors.New("account usage policy is waiting")
	ErrForbidden   = errors.New("account lacks permission")
	ErrChallenge   = errors.New("account requires manual verification")
	ErrSuperseded  = errors.New("account operation superseded")
	ErrUnsupported = errors.New("account transport is not configured")
)

type Key = contract.Key

// Credentials belongs to one authentication round. Callbacks must not retain or
// mutate it after returning. Values can hold refresh tokens; they are never sent.
type Credentials struct {
	Session      *htmlfetch.Session
	Headers      map[string]string
	Values       map[string]string
	BrowserState *browser.PageState // optional login result, published only after Check succeeds
}

// Context binds this candidate's CookieJar and credential headers for provider probes.
// The provider must restrict its own URLs and redirects to the account's scope.
func (c *Credentials) Context(ctx context.Context) context.Context {
	return htmlfetch.WithSession(core.WithHeaders(ctx, c.Headers), c.Session)
}

type Outcome string

const (
	Valid       Outcome = "valid"
	NeedsVerify Outcome = "needs-verify"
	Expired     Outcome = "expired"
	Forbidden   Outcome = "forbidden"
	Challenge   Outcome = "challenge"
	Unknown     Outcome = "unknown"
)

type Config struct {
	Origins []string
	// Login changes only its candidate. Check must return false,nil only for a
	// confirmed invalid session. Neither callback may reenter this account.
	Login    func(context.Context, *Credentials) error
	Check    func(context.Context, *Credentials) (bool, error)
	Classify func(*htmlfetch.Response, error) Outcome
	// Update optionally rotates token/CSRF after a valid response. It only mutates
	// its detached candidate, must be quick, and must not reenter Manager.
	Update func(*Credentials, *htmlfetch.Response) error
	// AuthHeaders adds credential header names to Authorization/X-CSRF-Token.
	AuthHeaders []string
	UserAgent   string
	// Empty ProxyURL pins HTTP to direct access. Inherited conflicting proxies fail.
	ProxyURL        string
	CheckInterval   time.Duration // zero: check on every call
	Timeout         time.Duration // default 30 seconds
	FailureCooldown time.Duration // default 5 seconds
	MaxConcurrent   int           // default 1; refresh itself is always single flight
	Store           Store
	Browser         *BrowserConfig
}

// BrowserConfig binds an account to a caller-owned pool. For stable proxy/UA
// identity use a dedicated configured pool; HTTP ProxyURL does not affect Rod.
type BrowserConfig struct {
	Pool       *browser.Pool
	URL        string // same-origin bootstrap page, used for snapshot restore/export
	Direct     bool
	Options    browser.PageOptions
	Login      func(context.Context, *rod.Page) error
	Check      func(context.Context, *rod.Page) (bool, error)
	CookieURLs []string                                             // browser-login bridge: cookies applicable to these URLs (default URL)
	Export     func(context.Context, *rod.Page, *Credentials) error // optional token/CSRF extraction
}

type Status struct {
	Key               Key       `json:"key"`
	Version           uint64    `json:"version"`
	Paused            bool      `json:"paused"`
	Authenticating    bool      `json:"authenticating"`
	HTTPReady         bool      `json:"http_ready"`
	BrowserReady      bool      `json:"browser_ready"`
	LastCheck         time.Time `json:"last_check"`
	LastLogin         time.Time `json:"last_login"`
	LastBrowserCheck  time.Time `json:"last_browser_check"`
	RetryAt           time.Time `json:"retry_at"`
	Failure           string    `json:"failure,omitempty"`
	HTTPConfigured    bool      `json:"http_configured"`
	BrowserConfigured bool      `json:"browser_configured"`
	InUse             int       `json:"in_use"`
	MaxConcurrent     int       `json:"max_concurrent"`
}

// Failure keeps the public message free of provider URLs/tokens while allowing
// errors.Is/As on the underlying cause. Do not log the unwrapped provider error.
type Failure struct {
	Code  string
	Cause error
}

func (e *Failure) Error() string { return "account: " + e.Code }
func (e *Failure) Unwrap() error { return e.Cause }

type flight struct {
	done        chan struct{}
	credentials *Credentials
	version     uint64
	err         error
}
type entry struct {
	mu                                          sync.Mutex
	config                                      Config
	key                                         Key
	manager                                     *Manager
	lastCandidateVersion                        uint64
	lastCandidateTransport                      Transport
	origins                                     map[string]bool
	authHeaders                                 map[string]bool
	slots                                       chan struct{}
	browserSlot                                 chan struct{}
	autoBridge                                  bool
	current                                     *Credentials
	seed                                        map[string]string
	pageState                                   *browser.PageState
	identityState                               *IdentityState
	version                                     uint64
	revision                                    uint64 // lifecycle epoch (load, pause, logout), independent of credential changes
	browserRevision                             uint64 // browser snapshot epoch, independent of HTTP generation
	loaded                                      bool
	paused                                      bool
	flight                                      *flight
	lastCheck, lastLogin, browserCheck, retryAt time.Time
	failure                                     error
}

// Manager owns no browser processes and starts no background goroutines.
type Manager struct {
	mu       sync.RWMutex
	client   *htmlfetch.Client
	entries  map[Key]*entry
	pools    *Pools
	identity *identityruntime.Manager
}

func NewManager(client *htmlfetch.Client) *Manager {
	return &Manager{client: client, entries: map[Key]*entry{}}
}

func defaultClassify(r *htmlfetch.Response, err error) Outcome {
	if r == nil {
		return Unknown
	}
	if err != nil {
		if _, ok := err.(*htmlfetch.StatusError); !ok {
			return Unknown
		}
	}
	switch r.StatusCode {
	case http.StatusUnauthorized:
		return NeedsVerify
	case http.StatusForbidden:
		return Forbidden
	}
	if err == nil && r.StatusCode >= 200 && r.StatusCode < 300 {
		return Valid
	}
	return Unknown
}
