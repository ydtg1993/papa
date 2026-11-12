package account

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account/contract"
	"sync"
	"time"
)

type PoolKey = contract.PoolKey

type Transport = contract.Transport

const (
	HTTP    Transport = "http"
	Browser Transport = "browser"
	Both    Transport = "both"
)

var (
	ErrPoolUnknown           = errors.New("account pool is not configured")
	ErrPoolConflict          = contract.ErrPoolConflict
	ErrPoolDisabled          = errors.New("account pool is disabled")
	ErrPoolClosed            = errors.New("account pool coordinator is closed")
	ErrPoolReferenced        = errors.New("account pool has task references or no reference guard")
	ErrAttemptChanged        = errors.New("account attempt identity is no longer current")
	ErrFixedProxyUnavailable = errors.New("account fixed proxy is unavailable")
)

const ProxyUnavailable Outcome = "proxy-unavailable"

type PoolConfig = contract.PoolConfig

type SwitchRecord = contract.SwitchRecord

// PoolState contains no credentials. Members are stored in stable selection order.
type PoolState = contract.PoolState

// ManagementState is separate from credential state. It never stores a third
// readiness flag: eligibility combines these exclusions with Manager auth state.
type ManagementState = contract.ManagementState

// PoolStore persists selection and management with compare-and-swap. Save must
// reject an existing version unequal to expected; expected zero creates only.
// Versions may advance by more than one when failures coalesce during an outage.
type PoolStore = contract.PoolStore

type PoolOptions struct {
	// Context attaches site request budgets to coordinator authentication traffic.
	Context           func(context.Context, Key) context.Context
	ScanInterval      time.Duration
	VerifyTimeout     time.Duration
	VerifyConcurrency int
	// ReverifyInterval is opt-in. Disabled and manual-challenge accounts are never
	// automatically restored. Expired cooldown merely permits another check.
	ReverifyInterval time.Duration
	References       func(context.Context, PoolKey) (int64, error)
}

type MemberSnapshot struct {
	Status
	Management ManagementState `json:"management"`
	State      string          `json:"state"`
	Persisted  bool            `json:"persisted"`
	Verifying  bool            `json:"verifying"`
}

type PoolSnapshot struct {
	PoolState
	Accounts         []MemberSnapshot `json:"accounts"`
	Available        int              `json:"available"`
	Waiting          int              `json:"waiting"`
	WaitReason       string           `json:"wait_reason,omitempty"`
	PersistenceError string           `json:"persistence_error,omitempty"`
}

// Attempt is an immutable identity receipt, not an account permit. Hold it in the
// task context; every exchange acquires the existing Manager account permit.
type Attempt struct {
	Pool              PoolKey   `json:"pool"`
	Account           Key       `json:"account"`
	SelectionVersion  uint64    `json:"selection_version"`
	ManagementVersion uint64    `json:"management_version"`
	CredentialVersion uint64    `json:"credential_version"`
	Transport         Transport `json:"transport"`
	owner             *Pools
	TaskID            uint64 `json:"task_id,omitempty"`
}

type attemptContextKey struct{}

func WithAttempt(ctx context.Context, a *Attempt) context.Context {
	return context.WithValue(ctx, attemptContextKey{}, a)
}
func AttemptFromContext(ctx context.Context) (*Attempt, bool) {
	a, ok := ctx.Value(attemptContextKey{}).(*Attempt)
	return a, ok && a != nil
}
func IsAttemptFailure(err error) bool {
	return errors.Is(err, ErrNeedsVerify) || errors.Is(err, ErrBudget) || errors.Is(err, ErrExpired) || errors.Is(err, ErrChallenge) || errors.Is(err, ErrAttemptChanged) || errors.Is(err, ErrPoolDisabled) || errors.Is(err, ErrPaused) || errors.Is(err, ErrFixedProxyUnavailable)
}

type managedAccount struct {
	state      ManagementState
	persisted  uint64
	dirty      bool
	verify     bool
	verifying  bool
	lastVerify time.Time
}
type managedPool struct {
	state             PoolState
	members           map[Key]struct{}
	waiting           int
	nextScan          time.Time
	persistenceError  string
	exhaustedNotified bool
	standbyNotified   bool
}

type poolWriteGate struct {
	once  sync.Once
	token chan struct{}
}

func (g *poolWriteGate) LockContext(ctx context.Context) error {
	g.once.Do(func() { g.token = make(chan struct{}, 1) })
	select {
	case g.token <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		g.Unlock()
		return err
	}
	return nil
}
func (g *poolWriteGate) Unlock() { <-g.token }

// Pools has one asynchronous coordinator per Manager. Lock order is Pools.mu
// then entry.mu. Network and store calls never execute with Pools.mu held.
type Pools struct {
	mu            sync.Mutex
	writes        poolWriteGate
	manager       *Manager
	store         PoolStore
	options       PoolOptions
	pools         map[PoolKey]*managedPool
	accounts      map[Key]*managedAccount
	wake          chan struct{}
	changed       chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	stopped       chan struct{}
	workers       chan struct{}
	activeSites   map[string]bool
	workerWG      sync.WaitGroup
	listeners     map[uint64]chan HealthEvent
	listenerID    uint64
	eventSequence uint64
}
