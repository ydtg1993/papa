package account

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

func NewPools(ctx context.Context, m *Manager, store PoolStore, options PoolOptions) (*Pools, error) {
	if m == nil || store == nil {
		return nil, errors.New("account manager and pool store required")
	}
	if m.Pools() != nil {
		return nil, errors.New("account pool coordinator already configured")
	}
	if options.ScanInterval < 0 || options.VerifyTimeout < 0 || options.ReverifyInterval < 0 || options.VerifyConcurrency < 0 || options.VerifyConcurrency > 64 {
		return nil, errors.New("negative account pool budget")
	}
	if options.ScanInterval == 0 {
		options.ScanInterval = time.Second
	}
	if options.VerifyTimeout == 0 {
		options.VerifyTimeout = 30 * time.Second
	}
	if options.VerifyConcurrency == 0 {
		options.VerifyConcurrency = 4
	}
	states, err := store.LoadPools(ctx)
	if err != nil {
		return nil, &Failure{Code: "pool-state-load-failed", Cause: err}
	}
	management, err := store.LoadManagement(ctx)
	if err != nil {
		return nil, &Failure{Code: "management-state-load-failed", Cause: err}
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Pools{manager: m, store: store, options: options, pools: map[PoolKey]*managedPool{}, accounts: map[Key]*managedAccount{}, wake: make(chan struct{}, 1), changed: make(chan struct{}), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	p.workers = make(chan struct{}, options.VerifyConcurrency)
	p.activeSites = map[string]bool{}
	p.stopped = make(chan struct{})
	p.listeners = map[uint64]chan HealthEvent{}
	for _, s := range management {
		if s.Version == 0 || strings.TrimSpace(s.Key.Site) == "" || strings.TrimSpace(s.Key.ID) == "" {
			cancel()
			return nil, errors.New("invalid account management state")
		}
		policy, policyErr := normalizePolicy(s.Policy)
		if policyErr != nil {
			cancel()
			return nil, policyErr
		}
		s.Policy = policy
		if s.Generation == 0 {
			s.Generation = s.Version
		}
		if s.LastControlVersion == 0 {
			s.LastControlVersion = s.Version
		}
		if s.LastControlVersion > s.Version {
			cancel()
			return nil, errors.New("invalid account control version")
		}
		s.Usage.Unit = "operation"
		for _, event := range s.Events {
			if event.Sequence > p.eventSequence {
				p.eventSequence = event.Sequence
			}
		}
		p.accounts[s.Key] = &managedAccount{state: cloneManagement(s), persisted: s.Version, verify: s.VerificationPending || s.Suspicious || (s.ProxyUnavailable && !s.Quarantined)}
	}
	for _, s := range states {
		if s.Revision == 0 {
			cancel()
			return nil, errors.New("invalid account pool state version")
		}
		if !s.Deleted {
			if err = p.validateConfig(&s.PoolConfig); err != nil {
				cancel()
				return nil, err
			}
			if s.CurrentID != "" {
				found := false
				for _, id := range s.Members {
					if id == s.CurrentID {
						found = true
					}
				}
				if !found || s.SelectionVersion == 0 {
					cancel()
					return nil, errors.New("invalid saved current account")
				}
			}
		}
		p.pools[s.Key] = poolEntry(s)
		for _, id := range s.Members {
			p.ensureManagement(Key{Site: s.Key.Site, ID: id})
		}
	}
	// A credential snapshot is not a successful check in this process. Revalidate
	// the saved current account first; do not rotate merely because of restart.
	for key := range p.accounts {
		if e, eerr := m.get(key); eerr == nil {
			e.mu.Lock()
			e.lastCheck = time.Time{}
			e.browserCheck = time.Time{}
			e.mu.Unlock()
		}
	}
	m.mu.Lock()
	if m.pools != nil {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("account pool coordinator already configured")
	}
	m.pools = p
	m.mu.Unlock()
	go func() { p.run(); p.workerWG.Wait(); close(p.stopped) }()
	return p, nil
}

func (m *Manager) Pools() *Pools { m.mu.RLock(); defer m.mu.RUnlock(); return m.pools }
func (p *Pools) Close() {
	_ = p.Shutdown(context.Background())
}

// Shutdown stops admission and waits for all verification callbacks. On timeout
// the caller must retain the browser and stores until a later Shutdown succeeds.
func (p *Pools) Shutdown(ctx context.Context) error {
	p.cancel()
	select {
	case <-p.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.manager.mu.Lock()
	if p.manager.pools == p {
		p.manager.pools = nil
	}
	p.manager.mu.Unlock()
	return nil
}
func (p *Pools) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *Pools) publishLocked() { close(p.changed); p.changed = make(chan struct{}) }
func poolEntry(s PoolState) *managedPool {
	q := &managedPool{state: clonePool(s), members: map[Key]struct{}{}}
	for _, id := range s.Members {
		q.members[Key{Site: s.Key.Site, ID: id}] = struct{}{}
	}
	return q
}
func clonePool(s PoolState) PoolState {
	s.Members = append([]string(nil), s.Members...)
	s.History = append([]SwitchRecord(nil), s.History...)
	return s
}
func (p *Pools) ensureManagement(key Key) *managedAccount {
	a := p.accounts[key]
	if a == nil {
		a = &managedAccount{state: ManagementState{Key: key, Version: 1, LastControlVersion: 1, Generation: 1, Policy: DefaultPolicy(), Usage: Usage{Unit: "operation"}, UpdatedAt: time.Now()}, dirty: true}
		p.accounts[key] = a
	}
	return a
}
func (p *Pools) validateConfig(c *PoolConfig) error {
	if strings.TrimSpace(c.Key.Site) == "" || strings.TrimSpace(c.Key.Name) == "" || len(c.Key.Site) > 256 || len(c.Key.Name) > 256 {
		return errors.New("account pool site and name required (max 256 bytes)")
	}
	if len(c.Members) > 1024 {
		return errors.New("account pool exceeds 1024 members")
	}
	if c.Transport == "" {
		c.Transport = HTTP
	}
	if c.Transport != HTTP && c.Transport != Browser && c.Transport != Both {
		return errors.New("invalid account pool transport")
	}
	if c.VerifyAttempts == 0 {
		c.VerifyAttempts = 1
	}
	if c.SwitchLimit == 0 {
		c.SwitchLimit = 16
	}
	if c.Cooldown == 0 {
		c.Cooldown = 5 * time.Second
	}
	if c.StandbyTarget < 0 || c.StandbyTarget > 1024 || c.StandbyTTL < 0 || c.StandbyTTL > 24*time.Hour {
		return errors.New("invalid standby budget")
	}
	if c.StandbyTarget > 0 && c.StandbyTTL == 0 {
		c.StandbyTTL = 15 * time.Minute
	}
	if c.VerifyAttempts < 1 || c.VerifyAttempts > 10 || c.SwitchLimit < 1 || c.SwitchLimit > 1024 || c.Cooldown < time.Millisecond || c.Cooldown > 24*time.Hour {
		return errors.New("invalid account pool compensation budget")
	}
	seen := map[string]bool{}
	for _, id := range c.Members {
		if seen[id] {
			return errors.New("duplicate account pool member")
		}
		seen[id] = true
		e, err := p.manager.get(Key{Site: c.Key.Site, ID: id})
		if err != nil {
			return ErrUnknown
		}
		if (c.Transport == HTTP || c.Transport == Both) && e.config.Login == nil {
			return ErrUnsupported
		}
		if (c.Transport == Browser || c.Transport == Both) && e.config.Browser == nil {
			return ErrUnsupported
		}
	}
	return nil
}

func (p *Pools) Upsert(ctx context.Context, c PoolConfig, expected uint64) (PoolSnapshot, error) {
	if err := p.validateConfig(&c); err != nil {
		return PoolSnapshot{}, err
	}
	if err := p.writes.LockContext(ctx); err != nil {
		return PoolSnapshot{}, err
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	q := p.pools[c.Key]
	var s PoolState
	if q != nil {
		s = clonePool(q.state)
	}
	if s.Revision != expected {
		p.mu.Unlock()
		return PoolSnapshot{}, ErrPoolConflict
	}
	s.PoolConfig = c
	s.Members = append([]string(nil), c.Members...)
	s.Revision++
	s.Deleted = false
	keep := false
	for _, id := range c.Members {
		if id == s.CurrentID {
			keep = true
		}
	}
	if s.CurrentID != "" && (!keep || c.Disabled) {
		p.switchState(&s, "", "pool-updated", 0)
	}
	p.mu.Unlock()
	if err := p.store.SavePoolCAS(ctx, s, expected); err != nil {
		return PoolSnapshot{}, &Failure{Code: "pool-state-save-failed", Cause: err}
	}
	p.mu.Lock()
	next := poolEntry(s)
	if q != nil {
		next.waiting = q.waiting
	}
	p.pools[c.Key] = next
	for _, id := range c.Members {
		p.ensureManagement(Key{Site: c.Key.Site, ID: id})
	}
	p.publishLocked()
	p.mu.Unlock()
	p.signal()
	return p.Get(c.Key)
}

// Delete keeps a tombstone so stale revision values never match a recreated pool.
// The embedding application must supply a durable task-reference guard.
func (p *Pools) Delete(ctx context.Context, key PoolKey, expected uint64) error {
	if err := p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	q := p.pools[key]
	if q == nil || q.state.Deleted {
		p.mu.Unlock()
		return ErrPoolUnknown
	}
	s := clonePool(q.state)
	p.mu.Unlock()
	if s.Revision != expected {
		return ErrPoolConflict
	}
	if !s.Disabled {
		return errors.New("disable account pool before deletion")
	}
	if p.options.References == nil {
		return ErrPoolReferenced
	}
	refs, err := p.options.References(ctx, key)
	if err != nil {
		return &Failure{Code: "pool-reference-check-failed", Cause: err}
	}
	if refs != 0 {
		return ErrPoolReferenced
	}
	s.Deleted = true
	s.Revision++
	if err = p.store.SavePoolCAS(ctx, s, expected); err != nil {
		return &Failure{Code: "pool-state-save-failed", Cause: err}
	}
	p.mu.Lock()
	p.pools[key] = poolEntry(s)
	p.publishLocked()
	p.mu.Unlock()
	return nil
}

func (p *Pools) SetDisabled(ctx context.Context, key Key, disabled bool, expected uint64) error {
	if _, err := p.manager.get(key); err != nil {
		return err
	}
	if err := p.writes.LockContext(ctx); err != nil {
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
	a.state.Disabled = disabled
	a.state.advanceControl()
	a.state.Generation++
	a.state.UpdatedAt = time.Now()
	a.dirty = true
	if disabled {
		a.state.Reason = "disabled"
		a.verify = false
		a.state.VerificationPending = false
	} else {
		a.state.Reason = "verification-required"
		a.state.Quarantined = true
		a.verify = true
		a.state.VerificationMode = RecoverLogin
		a.state.VerificationPending = true
	}
	p.wakeAccountLocked(key)
	p.publishLocked()
	p.mu.Unlock()
	p.signal()
	return p.flushManagement(ctx, key)
}

// Verify queues one bounded coordinator verification. GET snapshots never call it.
// A disabled account must be explicitly enabled first, which also queues a check.
func (p *Pools) Verify(ctx context.Context, key Key, expected uint64) error {
	return p.queueVerification(ctx, key, expected, RecoverLogin)
}

func ready(s Status, t Transport) bool {
	return !s.Paused && ((t == HTTP && s.HTTPReady) || (t == Browser && s.BrowserReady) || (t == Both && s.HTTPReady && s.BrowserReady))
}
func (p *Pools) status(key Key) Status {
	e, err := p.manager.get(key)
	if err != nil {
		return Status{Key: key}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return Status{Key: key, Version: e.version, Paused: e.paused, Authenticating: e.flight != nil, HTTPConfigured: e.config.Login != nil, BrowserConfigured: e.config.Browser != nil, HTTPReady: e.current != nil && !e.lastCheck.IsZero() && !e.paused && e.failure == nil, BrowserReady: e.pageState != nil && !e.browserCheck.IsZero() && !e.paused && e.failure == nil, LastCheck: e.lastCheck, LastBrowserCheck: e.browserCheck, LastLogin: e.lastLogin, RetryAt: e.retryAt, InUse: len(e.slots), MaxConcurrent: cap(e.slots)}
}
func (p *Pools) memberLocked(key Key, t Transport) MemberSnapshot {
	s := p.status(key)
	a := p.accounts[key]
	out := MemberSnapshot{Status: s, State: "pending"}
	if a == nil {
		return out
	}
	out.Management = cloneManagement(a.state)
	out.Verifying = a.verifying
	out.Persisted = !a.dirty
	switch {
	case a.state.Disabled || s.Paused:
		out.State = "disabled"
	case a.state.Suspicious:
		out.State = "needs-verification"
	case a.state.ProxyUnavailable:
		out.State = "proxy-unavailable"
	case a.verifying:
		out.State = "verifying"
	case time.Now().Before(a.state.CooldownUntil):
		out.State = "cooling"
	case a.state.Quarantined || a.state.PendingInvalidation:
		out.State = "invalid"
	case usageReason(a.state, time.Now()) != "":
		out.State = usageReason(a.state, time.Now())
	case a.dirty:
		out.State = "pending"
	case ready(s, t):
		out.State = "available"
	}
	return out
}
func (p *Pools) wakeAccountLocked(key Key) {
	for _, q := range p.pools {
		if _, ok := q.members[key]; ok {
			q.nextScan = time.Time{}
		}
	}
}
func (p *Pools) snapshotLocked(q *managedPool) PoolSnapshot {
	s := PoolSnapshot{PoolState: clonePool(q.state), Waiting: q.waiting, PersistenceError: q.persistenceError, Accounts: []MemberSnapshot{}}
	for _, id := range s.Members {
		a := p.memberLocked(Key{Site: s.Key.Site, ID: id}, s.Transport)
		s.Accounts = append(s.Accounts, a)
		if a.State == "available" {
			s.Available++
		}
		if !a.Persisted {
			s.PersistenceError = "management-state-not-saved"
		}
	}
	if s.Disabled {
		s.WaitReason = "pool-disabled"
	} else if s.CurrentID == "" {
		s.WaitReason = "no-verified-account"
	} else {
		a := p.memberLocked(Key{Site: s.Key.Site, ID: s.CurrentID}, s.Transport)
		if a.State != "available" {
			s.WaitReason = "current-account-" + a.State
		}
	}
	return s
}
func (p *Pools) Get(key PoolKey) (PoolSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.pools[key]
	if q == nil || q.state.Deleted {
		return PoolSnapshot{}, ErrPoolUnknown
	}
	return p.snapshotLocked(q), nil
}
func (p *Pools) Snapshot() []PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PoolSnapshot, 0, len(p.pools))
	for _, q := range p.pools {
		if !q.state.Deleted {
			out = append(out, p.snapshotLocked(q))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Site == out[j].Key.Site {
			return out[i].Key.Name < out[j].Key.Name
		}
		return out[i].Key.Site < out[j].Key.Site
	})
	return out
}

func (p *Pools) Acquire(ctx context.Context, key PoolKey, t Transport) (*Attempt, error) {
	counted := false
	defer func() {
		if counted {
			p.mu.Lock()
			if q := p.pools[key]; q != nil {
				q.waiting--
			}
			p.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.ctx.Err() != nil {
			return nil, ErrPoolClosed
		}
		p.mu.Lock()
		q := p.pools[key]
		if q == nil || q.state.Deleted {
			p.mu.Unlock()
			return nil, ErrPoolUnknown
		}
		if t == "" {
			t = q.state.Transport
		}
		if t != HTTP && t != Browser && t != Both {
			p.mu.Unlock()
			return nil, ErrUnsupported
		}
		if q.state.Transport != Both && t != q.state.Transport {
			p.mu.Unlock()
			return nil, ErrUnsupported
		}
		k := Key{Site: key.Site, ID: q.state.CurrentID}
		a := p.accounts[k]
		if !q.state.Disabled && a != nil && p.memberLocked(k, t).State == "available" {
			s := p.status(k)
			out := &Attempt{Pool: key, Account: k, SelectionVersion: q.state.SelectionVersion, ManagementVersion: a.state.Generation, CredentialVersion: s.Version, Transport: t, owner: p}
			p.mu.Unlock()
			return out, nil
		}
		if !counted {
			q.waiting++
			counted = true
		}
		changed := p.changed
		p.mu.Unlock()
		p.signal()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, ErrPoolClosed
		case <-changed:
		}
	}
}

func (p *Pools) ValidateAttempt(a *Attempt) error {
	if a == nil || a.owner != p {
		return ErrAttemptChanged
	}
	if p.ctx.Err() != nil {
		return ErrPoolClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.validateAttemptLocked(a)
}
func (p *Pools) validateAttemptLocked(a *Attempt) error {
	q := p.pools[a.Pool]
	m := p.accounts[a.Account]
	if q == nil || q.state.Deleted || q.state.Disabled || q.state.CurrentID != a.Account.ID || q.state.SelectionVersion != a.SelectionVersion || m == nil || m.state.Generation != a.ManagementVersion || m.state.Disabled || m.state.Quarantined || m.state.Suspicious || m.state.ProxyUnavailable || m.verifying {
		return ErrAttemptChanged
	}
	if p.status(a.Account).Version != a.CredentialVersion {
		return ErrAttemptChanged
	}
	return nil
}

// ReportFailure is memory-only and returns immediately. The coordinator scans
// these dirty records, retries persistence, then chooses replacements. Ordinary
// 403, timeout and parsing failures never change selection.
func (p *Pools) ReportFailure(a *Attempt, outcome Outcome, taskID uint64) bool {
	if a == nil {
		return false
	}
	return p.reportFailure(a, outcome, taskID, a.Transport)
}
func (p *Pools) reportFailure(a *Attempt, outcome Outcome, taskID uint64, t Transport) bool {
	if a == nil || a.owner != p || (outcome != NeedsVerify && outcome != Expired && outcome != Challenge && outcome != ProxyUnavailable) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.validateAttemptLocked(a) != nil {
		return false
	}
	e, _ := p.manager.get(a.Account)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.version != a.CredentialVersion {
		return false
	}
	m := p.accounts[a.Account]
	now := time.Now()
	m.state.advanceControl()
	m.state.Generation++
	m.state.UpdatedAt = now
	m.state.TriggerTaskID = taskID
	m.state.CredentialVersion = e.version
	m.state.BrowserVersion = e.browserRevision
	m.state.CheckTransport = t
	m.state.InvalidChecks = 0
	m.state.UnknownChecks = 0
	m.state.ProxyChecks = 0
	m.state.SuspectSince = now
	m.state.VerificationMode = CheckExisting
	switch outcome {
	case NeedsVerify:
		m.state.Suspicious = true
		m.state.Reason = "needs-verification"
		m.state.NextVerifyAt = now.Add(m.state.Policy.Buffer)
		m.verify = true
		m.state.VerificationPending = true
	case ProxyUnavailable:
		m.state.ProxyUnavailable = true
		m.state.Reason = "fixed-proxy-unavailable"
		m.state.NextVerifyAt = now.Add(m.state.Policy.Buffer)
		m.verify = true
		m.state.VerificationPending = true
	case Challenge:
		m.state.Quarantined = true
		m.state.Reason = "manual-verification"
		m.verify = false
		m.state.VerificationPending = false
	case Expired:
		m.state.Quarantined = true
		m.state.PendingInvalidation = true
		m.state.Reason = "expired"
		m.verify = false
		m.state.VerificationPending = false
		if t == HTTP || t == Both {
			if e.current != nil {
				e.seed = e.current.Values
			}
			e.current = nil
			e.lastCheck = time.Time{}
			e.version++
		}
		if t == Browser || t == Both {
			e.pageState = nil
			e.browserCheck = time.Time{}
			e.browserRevision++
		}
	}
	m.dirty = true
	p.eventLocked(m, m.state.Reason, m.state.Reason)
	p.wakeAccountLocked(a.Account)
	p.publishLocked()
	p.signal()
	return true
}

func (p *Pools) flushManagement(ctx context.Context, key Key) error {
	p.mu.Lock()
	a := p.accounts[key]
	if a == nil || !a.dirty {
		p.mu.Unlock()
		return nil
	}
	s, expected := cloneManagement(a.state), a.persisted
	p.mu.Unlock()
	if err := p.store.SaveManagementCAS(ctx, s, expected); err != nil {
		return &Failure{Code: "management-state-save-failed", Cause: err}
	}
	p.mu.Lock()
	a = p.accounts[key]
	a.persisted = s.Version
	a.dirty = a.state.Version != s.Version
	p.publishLocked()
	p.mu.Unlock()
	return nil
}

func (p *Pools) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.options.ScanInterval)
	defer ticker.Stop()
	for {
		p.reconcile()
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
			p.mu.Lock()
			p.publishLocked()
			p.mu.Unlock()
		}
	}
}

func (p *Pools) reconcile() {
	p.mu.Lock()
	keys := make([]Key, 0, len(p.accounts))
	pkeys := make([]PoolKey, 0, len(p.pools))
	for key := range p.accounts {
		keys = append(keys, key)
	}
	for key := range p.pools {
		pkeys = append(pkeys, key)
	}
	p.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Site == keys[j].Site {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].Site < keys[j].Site
	})
	sort.Slice(pkeys, func(i, j int) bool {
		if pkeys[i].Site == pkeys[j].Site {
			return pkeys[i].Name < pkeys[j].Name
		}
		return pkeys[i].Site < pkeys[j].Site
	})
	for _, key := range keys {
		if p.ctx.Err() != nil {
			return
		}
		if p.writes.LockContext(p.ctx) != nil {
			return
		}
		_ = p.flushManagement(p.ctx, key)
		p.writes.Unlock()
		p.mu.Lock()
		a := p.accounts[key]
		verify := (a.verify || a.state.VerificationPending || a.state.Suspicious || (a.state.ProxyUnavailable && !a.state.Quarantined)) && !time.Now().Before(a.state.NextVerifyAt)
		recovery := a.state.Policy.RecoveryInterval
		if recovery == 0 {
			recovery = p.options.ReverifyInterval
		}
		auto := recovery > 0 && a.state.Quarantined && a.state.Reason != "manual-verification" && !p.status(key).Paused && !a.state.Suspicious && time.Since(a.state.UpdatedAt) >= recovery && !time.Now().Before(a.state.CooldownUntil)
		if a.state.Disabled || a.dirty {
			verify = false
			auto = false
		}
		p.mu.Unlock()
		if verify || auto {
			t := p.requiredTransport(key)
			if t != "" {
				p.verifyAccount(key, t, 1, 5*time.Second, true)
			}
		}
	}
	for _, key := range pkeys {
		if p.ctx.Err() != nil {
			return
		}
		p.reconcilePool(key)
	}
}

func (p *Pools) requiredTransport(key Key) Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	var t Transport
	for _, q := range p.pools {
		if q.state.Deleted {
			continue
		}
		if _, ok := q.members[key]; ok {
			if t == "" {
				t = q.state.Transport
			} else if t != q.state.Transport {
				t = Both
			}
		}
	}
	if t == "" {
		if e, err := p.manager.get(key); err == nil {
			if e.config.Login != nil {
				t = HTTP
			}
			if e.config.Browser != nil {
				if t == HTTP {
					t = Both
				} else {
					t = Browser
				}
			}
		}
	}
	return t
}

// verifyAccount schedules work on the existing coordinator's bounded worker set.
// Account-level coalescing prevents multiple pools from checking the same state.
func (p *Pools) verifyAccount(key Key, t Transport, attempts int, cooldown time.Duration, explicit bool) {
	p.mu.Lock()
	a := p.accounts[key]
	if a == nil || a.state.Disabled || a.verifying || p.activeSites[key.Site] || a.dirty || time.Now().Before(a.state.NextVerifyAt) || (!explicit && (p.status(key).Paused || a.state.Quarantined || a.state.Suspicious || a.state.ProxyUnavailable || time.Now().Before(a.state.CooldownUntil))) {
		p.mu.Unlock()
		return
	}
	select {
	case p.workers <- struct{}{}:
	default:
		p.mu.Unlock()
		return
	}
	a.verifying = true
	p.activeSites[key.Site] = true
	a.verify = false
	mode := a.state.VerificationMode
	if explicit && a.state.Quarantined && !a.state.Suspicious && !a.state.VerificationPending {
		mode = RecoverLogin
		if a.state.ProxyUnavailable {
			mode = CheckExisting
		}
	}
	if mode == "" {
		mode = RecoverLogin
	}
	if a.state.CheckTransport != "" {
		t = a.state.CheckTransport
	}
	state := cloneManagement(a.state)
	p.workerWG.Add(1)
	p.publishLocked()
	p.mu.Unlock()
	go func() {
		defer p.workerWG.Done()
		defer func() { <-p.workers; p.mu.Lock(); delete(p.activeSites, key.Site); p.mu.Unlock(); p.signal() }()
		var err error = &Failure{Code: "verification-aborted"}
		var checkedVersion, checkedBrowser uint64
		defer func() {
			if recover() != nil {
				err = &Failure{Code: "verification-panic"}
			}
			p.finishVerification(key, t, state, mode, checkedVersion, checkedBrowser, err, cooldown)
		}()
		timeout := state.Policy.VerifyTimeout
		if p.options.VerifyTimeout < timeout {
			timeout = p.options.VerifyTimeout
		}
		ctx, cancel := context.WithTimeout(p.ctx, timeout)
		defer cancel()
		if p.options.Context != nil {
			ctx = p.options.Context(ctx, key)
		}
		if mode == CheckExisting {
			checkedVersion, checkedBrowser, err = p.checkTransport(ctx, key, t, state.CredentialVersion, state.BrowserVersion)
		} else {
			for i := 0; i < attempts; i++ {
				err = p.verifyTransport(ctx, key, t)
				if err == nil || errors.Is(err, ErrChallenge) || errors.Is(err, ErrBudget) || ctx.Err() != nil {
					break
				}
			}
		}
		if err == nil {
			err = ctx.Err()
		}
	}()
}

func (p *Pools) verifyTransport(ctx context.Context, key Key, t Transport) (err error) {
	ctx = context.WithValue(ctx, coordinatorContextKey{}, true)
	defer func() {
		if recover() != nil {
			err = &Failure{Code: "verification-panic"}
		}
	}()
	ctx, releaseIdentity, identityErr := p.manager.PinIdentity(ctx, key)
	if identityErr != nil {
		return identityErr
	}
	defer releaseIdentity()
	e, err := p.manager.get(key)
	if err != nil {
		return err
	}
	if err = p.manager.Resume(key); err != nil {
		return err
	}
	if t == HTTP || t == Both {
		ctx, err = e.context(ctx)
		if err != nil {
			return err
		}
		if err = e.enter(ctx); err != nil {
			return err
		}
		func() {
			defer e.leave()
			if err = e.load(ctx); err == nil {
				if err = e.validateIdentityState(ctx); err == nil {
					_, _, err = e.ensure(ctx)
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	if t == Browser || t == Both {
		err = p.manager.WithBrowser(ctx, key, func(*rod.Page) error { return nil })
	}
	return err
}

func (p *Pools) reconcilePool(key PoolKey) {
	p.mu.Lock()
	q := p.pools[key]
	if q == nil || q.state.Deleted || q.state.Disabled || time.Now().Before(q.nextScan) {
		p.mu.Unlock()
		return
	}
	s := clonePool(q.state)
	current := Key{Site: key.Site, ID: s.CurrentID}
	if s.CurrentID != "" {
		member := p.memberLocked(current, s.Transport)
		a := p.accounts[current]
		if member.State == "available" {
			p.mu.Unlock()
			p.preverifyStandbys(s)
			return
		}
		if a != nil && !a.state.Disabled && (a.state.Suspicious || (a.state.ProxyUnavailable && !a.state.Quarantined) || a.verifying || usageReason(a.state, time.Now()) != "") {
			p.mu.Unlock()
			return
		}
	}
	ids := append([]string(nil), s.Members...)
	if s.CurrentID != "" {
		ids = append([]string{s.CurrentID}, ids...)
	}
	p.mu.Unlock()
	selected := ""
	seen := map[string]bool{}
	checked := 0
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		k := Key{Site: key.Site, ID: id}
		p.mu.Lock()
		a := p.accounts[k]
		member := p.memberLocked(k, s.Transport)
		pending := a != nil && !a.dirty && !member.Paused && !a.state.Disabled && !a.state.Quarantined && !a.state.Suspicious && !a.state.ProxyUnavailable && !a.state.PendingInvalidation && !time.Now().Before(a.state.CooldownUntil)
		p.mu.Unlock()
		if member.State == "available" {
			selected = id
			break
		}
		if pending {
			if checked >= s.SwitchLimit {
				break
			}
			checked++
			p.verifyAccount(k, s.Transport, s.VerifyAttempts, s.Cooldown, false)
			p.mu.Lock()
			member = p.memberLocked(k, s.Transport)
			p.mu.Unlock()
			if member.State == "available" {
				selected = id
				break
			}
			return
		}
	}
	if p.writes.LockContext(p.ctx) != nil {
		return
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	q = p.pools[key]
	if q == nil || q.state.Revision != s.Revision || q.state.Disabled {
		p.mu.Unlock()
		return
	}
	q.nextScan = time.Now().Add(s.Cooldown)
	if selected == "" && !q.exhaustedNotified && len(s.Members) > 0 {
		a := p.accounts[Key{Site: key.Site, ID: s.Members[0]}]
		if a != nil {
			a.state.Version++
			a.dirty = true
			p.eventForPoolLocked(a, key, "pool-exhausted", "no-verified-account")
			q.exhaustedNotified = true
		}
	}
	if selected != "" {
		q.exhaustedNotified = false
	}
	if q.state.CurrentID == selected {
		p.mu.Unlock()
		return
	}
	reason := "initial-selection"
	var taskID uint64
	if a := p.accounts[current]; a != nil && s.CurrentID != "" {
		reason = a.state.Reason
		taskID = a.state.TriggerTaskID
	}
	if reason == "" {
		reason = "account-unavailable"
	}
	next := clonePool(q.state)
	next.Revision++
	p.switchState(&next, selected, reason, taskID)
	p.mu.Unlock()
	if err := p.store.SavePoolCAS(p.ctx, next, s.Revision); err != nil {
		p.mu.Lock()
		q.persistenceError = "pool-state-not-saved"
		p.publishLocked()
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	q.state = next
	q.persistenceError = ""
	p.publishLocked()
	p.mu.Unlock()
}

func (p *Pools) switchState(s *PoolState, id, reason string, taskID uint64) {
	s.SelectionVersion++
	s.History = append(s.History, SwitchRecord{From: s.CurrentID, To: id, Reason: reason, TaskID: taskID, SelectionVersion: s.SelectionVersion, At: time.Now()})
	s.CurrentID = id
	if len(s.History) > 100 {
		s.History = append([]SwitchRecord(nil), s.History[len(s.History)-100:]...)
	}
}
