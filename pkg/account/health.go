package account

import (
	"context"
	"errors"
	"sort"
	"time"
)

type VerificationMode string

const (
	CheckExisting VerificationMode = "check-existing"
	RecoverLogin  VerificationMode = "recover-login"
)

// Policy applies to one account across all pools. Usage counts accepted public
// HTTP exchanges and browser operations, never the browser's network requests.
// Zero health durations select defaults; zero usage limits disable the limit.
type Policy struct {
	Buffer               time.Duration `json:"buffer"`
	InvalidConfirmations int           `json:"invalid_confirmations"`
	CheckInterval        time.Duration `json:"check_interval"`
	VerifyTimeout        time.Duration `json:"verify_timeout"`
	UnknownBackoff       time.Duration `json:"unknown_backoff"`
	MaxBackoff           time.Duration `json:"max_backoff"`
	UnknownAlertAfter    time.Duration `json:"unknown_alert_after"`
	OperationInterval    time.Duration `json:"operation_interval"`
	DailyLimit           int64         `json:"daily_limit"`
	Timezone             string        `json:"timezone"`
	WorkStartMinute      int           `json:"work_start_minute"`
	WorkEndMinute        int           `json:"work_end_minute"`
	LoginInterval        time.Duration `json:"login_interval"`
	RecoveryInterval     time.Duration `json:"recovery_interval"`
	TrackUsage           bool          `json:"track_usage"`
}

type Usage struct {
	Unit             string    `json:"unit"`
	Day              string    `json:"day"`
	Operations       int64     `json:"operations"`
	LastOperation    time.Time `json:"last_operation"`
	LastLoginAttempt time.Time `json:"last_login_attempt"`
}

func DefaultPolicy() Policy {
	return Policy{Buffer: 10 * time.Second, InvalidConfirmations: 2, CheckInterval: 10 * time.Second, VerifyTimeout: 30 * time.Second, UnknownBackoff: 10 * time.Second, MaxBackoff: 5 * time.Minute, UnknownAlertAfter: 5 * time.Minute, Timezone: "UTC"}
}

func normalizePolicy(v Policy) (Policy, error) {
	d := DefaultPolicy()
	if v.Buffer == 0 {
		v.Buffer = d.Buffer
	}
	if v.InvalidConfirmations == 0 {
		v.InvalidConfirmations = d.InvalidConfirmations
	}
	if v.CheckInterval == 0 {
		v.CheckInterval = d.CheckInterval
	}
	if v.VerifyTimeout == 0 {
		v.VerifyTimeout = d.VerifyTimeout
	}
	if v.UnknownBackoff == 0 {
		v.UnknownBackoff = d.UnknownBackoff
	}
	if v.MaxBackoff == 0 {
		v.MaxBackoff = d.MaxBackoff
	}
	if v.UnknownAlertAfter == 0 {
		v.UnknownAlertAfter = d.UnknownAlertAfter
	}
	if v.Timezone == "" {
		v.Timezone = d.Timezone
	}
	if v.Buffer < time.Millisecond || v.Buffer > 24*time.Hour || v.InvalidConfirmations < 1 || v.InvalidConfirmations > 10 || v.CheckInterval < time.Millisecond || v.CheckInterval > 24*time.Hour || v.VerifyTimeout < time.Millisecond || v.VerifyTimeout > 10*time.Minute || v.UnknownBackoff < time.Millisecond || v.MaxBackoff < v.UnknownBackoff || v.MaxBackoff > 24*time.Hour || v.UnknownAlertAfter < time.Millisecond || v.OperationInterval < 0 || v.LoginInterval < 0 || v.RecoveryInterval < 0 || v.RecoveryInterval > 30*24*time.Hour || v.DailyLimit < 0 || v.WorkStartMinute < 0 || v.WorkStartMinute >= 1440 || v.WorkEndMinute < 0 || v.WorkEndMinute >= 1440 {
		return v, errors.New("invalid account policy")
	}
	if _, err := time.LoadLocation(v.Timezone); err != nil {
		return v, errors.New("invalid account policy timezone")
	}
	return v, nil
}

// HealthEvent contains only framework-defined reason codes and identity keys.
// No callback error text, credential, request URL or provider payload is stored.
type HealthEvent struct {
	Sequence uint64    `json:"sequence"`
	Key      Key       `json:"key"`
	Pool     PoolKey   `json:"pool,omitempty"`
	Kind     string    `json:"kind"`
	Reason   string    `json:"reason"`
	TaskID   uint64    `json:"task_id,omitempty"`
	At       time.Time `json:"at"`
	Version  uint64    `json:"version"`
}

func cloneManagement(s ManagementState) ManagementState {
	s.Events = append([]HealthEvent(nil), s.Events...)
	return s
}

// A snapshot remains a valid control precondition across usage-only writes.
// Storage CAS still uses the exact latest persisted Version, never this range.
func matchesControlVersion(a *managedAccount, expected uint64) bool {
	if a == nil {
		return expected == 0
	}
	last := a.state.LastControlVersion
	if last == 0 {
		last = a.state.Version
	}
	return expected >= last && expected <= a.state.Version
}

func (s *ManagementState) advanceControl() {
	s.Version++
	s.LastControlVersion = s.Version
}

func (p *Pools) eventLocked(a *managedAccount, kind, reason string) {
	p.eventForPoolLocked(a, PoolKey{}, kind, reason)
}

func (p *Pools) eventForPoolLocked(a *managedAccount, pool PoolKey, kind, reason string) {
	p.eventSequence++
	e := HealthEvent{Sequence: p.eventSequence, Key: a.state.Key, Pool: pool, Kind: kind, Reason: reason, TaskID: a.state.TriggerTaskID, At: time.Now(), Version: a.state.Version}
	a.state.Events = append(a.state.Events, e)
	if len(a.state.Events) > 100 {
		a.state.Events = append([]HealthEvent(nil), a.state.Events[len(a.state.Events)-100:]...)
	}
	for _, ch := range p.listeners {
		select {
		case ch <- e:
		default:
		}
	}
}

// AddEventListener delivers best-effort notifications through a bounded queue.
// A slow listener cannot block scheduling. Events is the durable query API.
func (p *Pools) AddEventListener(fn func(HealthEvent)) func() {
	if fn == nil {
		return func() {}
	}
	ch := make(chan HealthEvent, 32)
	stop := make(chan struct{})
	p.mu.Lock()
	p.listenerID++
	id := p.listenerID
	p.listeners[id] = ch
	p.mu.Unlock()
	go func() {
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-stop:
				return
			case e := <-ch:
				func() { defer func() { _ = recover() }(); fn(e) }()
			}
		}
	}()
	return func() {
		p.mu.Lock()
		if _, ok := p.listeners[id]; ok {
			delete(p.listeners, id)
			close(stop)
		}
		p.mu.Unlock()
	}
}

// Events returns at most 100 recent events after a sequence; Key{} queries all
// accounts. Persistence retains the newest 100 per account, not an audit ledger.
func (p *Pools) Events(key Key, after uint64) []HealthEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []HealthEvent{}
	for k, a := range p.accounts {
		if key != (Key{}) && key != k {
			continue
		}
		for _, e := range a.state.Events {
			if e.Sequence > after {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	if len(out) > 100 {
		out = out[len(out)-100:]
	}
	return out
}

func (p *Pools) Policy(key Key) (Policy, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.accounts[key]
	if a == nil {
		if _, err := p.manager.get(key); err != nil {
			return Policy{}, 0, err
		}
		return DefaultPolicy(), 0, nil
	}
	return a.state.Policy, a.state.Version, nil
}

// SetPolicy persists before reporting success; failures retain a conservative
// pending state and are retried by the same coordinator.
func (p *Pools) SetPolicy(ctx context.Context, key Key, value Policy, expected uint64) error {
	v, err := normalizePolicy(value)
	if err != nil {
		return err
	}
	if _, err = p.manager.get(key); err != nil {
		return err
	}
	if err = p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	a := p.accounts[key]
	if !matchesControlVersion(a, expected) {
		p.mu.Unlock()
		return ErrPoolConflict
	}
	a = p.ensureManagement(key)
	a.state.Policy = v
	if v.LoginInterval > 0 {
		if e, getErr := p.manager.get(key); getErr == nil {
			e.mu.Lock()
			if e.lastLogin.After(a.state.Usage.LastLoginAttempt) {
				a.state.Usage.LastLoginAttempt = e.lastLogin
			}
			e.mu.Unlock()
		}
	}
	a.state.advanceControl()
	a.state.UpdatedAt = time.Now()
	a.dirty = true
	p.publishLocked()
	p.mu.Unlock()
	err = p.flushManagement(ctx, key)
	p.signal()
	return err
}

// Readiness never logs in, selects an account, consumes a permit or writes state.
func (p *Pools) Readiness(key PoolKey) (bool, string) {
	if p.ctx.Err() != nil {
		return false, "pool-closed"
	}
	s, err := p.Get(key)
	if err != nil {
		return false, "pool-unknown"
	}
	return s.WaitReason == "", s.WaitReason
}

func (p *Pools) CheckExisting(ctx context.Context, key Key, expected uint64) error {
	return p.queueVerification(ctx, key, expected, CheckExisting)
}

func (p *Pools) queueVerification(ctx context.Context, key Key, expected uint64, mode VerificationMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e, err := p.manager.get(key)
	if err != nil {
		return err
	}
	if err = p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	a := p.accounts[key]
	if !matchesControlVersion(a, expected) || (a != nil && (a.verifying || (a.verify && a.state.VerificationMode == mode))) {
		p.mu.Unlock()
		return ErrPoolConflict
	}
	a = p.ensureManagement(key)
	if a.state.Disabled {
		p.mu.Unlock()
		return ErrPaused
	}
	e.mu.Lock()
	v, bv := e.version, e.browserRevision
	e.mu.Unlock()
	a.state.advanceControl()
	a.state.Generation++
	a.state.UpdatedAt = time.Now()
	a.state.VerificationMode = mode
	a.state.VerificationPending = true
	a.state.CredentialVersion = v
	a.state.BrowserVersion = bv
	a.state.CheckTransport = ""
	a.state.NextVerifyAt = time.Now()
	a.state.InvalidChecks = 0
	a.state.UnknownChecks = 0
	if mode == CheckExisting {
		a.state.Suspicious = true
		a.state.SuspectSince = time.Now()
		a.state.Reason = "verification-requested"
	} else {
		a.state.Quarantined = true
		a.state.Suspicious = false
		a.state.Reason = "recovery-requested"
	}
	a.verify = true
	a.dirty = true
	p.wakeAccountLocked(key)
	p.publishLocked()
	p.mu.Unlock()
	p.signal()
	return p.flushManagement(ctx, key)
}

func (p *Pools) guardAccount(key Key) error {
	if p.ctx.Err() != nil {
		return ErrPoolClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.accounts[key]
	if a == nil {
		return nil
	}
	if a.state.Disabled {
		return ErrPaused
	}
	if a.state.Suspicious {
		return ErrNeedsVerify
	}
	if a.state.ProxyUnavailable {
		return ErrFixedProxyUnavailable
	}
	if a.state.Quarantined {
		return ErrExpired
	}
	return nil
}
