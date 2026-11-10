package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnavailable    = errors.New("proxy pool has no eligible proxy")
	ErrClosed         = errors.New("proxy manager is closed")
	ErrUnknownProxy   = errors.New("unknown proxy identifier")
	ErrHealthDisabled = errors.New("proxy health is disabled")
	ErrHealthCapacity = errors.New("proxy health state capacity reached")
	ErrConflict       = errors.New("proxy health version conflict")
)

// MaxFixedHealthEntries bounds separately registered fixed-proxy health records.
// Provider records are retained only while listed or referenced by an in-flight use.
const MaxFixedHealthEntries = 4096

// HealthConfig is disabled by default. Limits apply to new uses; existing uses
// finish normally after a limit or policy change.
type HealthConfig struct {
	Enabled           bool          `json:"enabled" yaml:"enabled"`
	FailureThreshold  int           `json:"failure_threshold" yaml:"failure_threshold"`
	InitialCooldown   time.Duration `json:"initial_cooldown" yaml:"initial_cooldown"`
	MaxCooldown       time.Duration `json:"max_cooldown" yaml:"max_cooldown"`
	MaxProbes         int           `json:"max_probes" yaml:"max_probes"`
	RecoverySuccesses int           `json:"recovery_successes" yaml:"recovery_successes"`
}

func DefaultHealthConfig() HealthConfig {
	return HealthConfig{FailureThreshold: 3, InitialCooldown: 30 * time.Second,
		MaxCooldown: 10 * time.Minute, MaxProbes: 1, RecoverySuccesses: 2}
}

func (c HealthConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.FailureThreshold < 1 || c.FailureThreshold > 10000 || c.MaxProbes < 1 || c.MaxProbes > 10000 || c.RecoverySuccesses < 1 || c.RecoverySuccesses > 10000 {
		return errors.New("proxy health thresholds and probe limit must be between 1 and 10000")
	}
	if c.InitialCooldown <= 0 || c.MaxCooldown < c.InitialCooldown || c.MaxCooldown > 24*time.Hour {
		return errors.New("proxy cooldown must be positive, ordered, and at most 24 hours")
	}
	return nil
}

func (m *Manager) ConfigureHealth(c HealthConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	m.initLocked()
	// Ignore feedback from uses acquired under a different enabled state.
	if c.Enabled != m.health.Enabled {
		for _, e := range m.entries {
			e.version++
			e.revision++
		}
	}
	m.health = c
	m.signalLocked()
	return nil
}

func (m *Manager) HealthConfig() HealthConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.health
}

type State string

const (
	StateAvailable State = "available"
	StateCooling   State = "cooling"
	StateProbing   State = "probing"
	StateDisabled  State = "disabled"
)

// FailureKind is deliberately classified by the caller. Target HTTP failures,
// account failures and rate limiting must not be reported as proxy failures.
type FailureKind string

const (
	Neutral               FailureKind = "neutral"
	Success               FailureKind = "success"
	TransportFailure      FailureKind = "transport_failure"
	AuthenticationFailure FailureKind = "proxy_authentication_failure"
	SiteBlocked           FailureKind = "site_blocked"
)

type Feedback struct {
	Kind FailureKind
}

type healthEntry struct {
	id          string
	address     string
	active      bool
	fixed       bool
	state       State
	version     uint64
	revision    uint64
	failures    int
	successes   int
	cooldown    time.Duration
	until       time.Time
	inflight    int
	probes      int
	lastUsed    time.Time
	lastUpdated time.Time
	lastResult  FailureKind
}

// HealthSnapshot contains no address, userinfo or arbitrary error text. IDs are
// opaque and suitable for administration endpoints and audit logs.
type HealthSnapshot struct {
	ID                  string        `json:"id"`
	State               State         `json:"state"`
	Active              bool          `json:"active"`
	Fixed               bool          `json:"fixed"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
	RecoverySuccesses   int           `json:"recovery_successes"`
	CooldownUntil       time.Time     `json:"cooldown_until,omitempty"`
	CooldownRemaining   time.Duration `json:"cooldown_remaining"`
	InFlight            int           `json:"inflight"`
	Probes              int           `json:"probes"`
	LastUpdated         time.Time     `json:"last_updated,omitempty"`
	LastResult          FailureKind   `json:"last_result,omitempty"`
	Version             uint64        `json:"version"`
}

// Reference binds feedback to the proxy and health generation actually used.
// Exactly one of Report, ReportHTTP or Release completes it. All are idempotent.
// The URL is only for transport construction; never put it in snapshots or logs.
type Reference struct {
	manager *Manager
	entry   *healthEntry
	address string
	version uint64
	probe   bool
	tracked bool
	once    sync.Once
}

func (r *Reference) URL() string {
	if r == nil {
		return ""
	}
	return r.address
}

func (r *Reference) ID() string {
	if r == nil || r.address == "" {
		return ""
	}
	return opaqueID(r.address)
}

func (r *Reference) Release() { r.Report(Feedback{Kind: Neutral}) }

func (r *Reference) Report(feedback Feedback) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.manager == nil || r.entry == nil {
			return
		}
		m := r.manager
		m.mu.Lock()
		defer m.mu.Unlock()
		e := r.entry
		e.inflight--
		if r.probe {
			e.probes--
			m.probes--
		}
		if r.tracked && m.health.Enabled && r.version == e.version && e.state != StateDisabled {
			m.feedbackLocked(e, r.probe, feedback.Kind)
		}
		m.pruneLocked()
		m.signalLocked()
	})
}

// ReportHTTP only infers failures when attribution to this proxy is known.
// Cancellation, ordinary target timeouts, 403, 429 and account errors are neutral.
func (r *Reference) ReportHTTP(status int, err error) {
	if r == nil {
		return
	}
	r.Report(Feedback{Kind: ClassifyHTTP(r.address, status, err)})
}

func ClassifyHTTP(proxyURL string, status int, err error) FailureKind {
	if proxyURL == "" {
		return Neutral
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Neutral
	}
	if status == http.StatusProxyAuthRequired {
		return AuthenticationFailure
	}
	var known interface{ ProxyFailure() string }
	if errors.As(err, &known) {
		switch known.ProxyFailure() {
		case "transport":
			return TransportFailure
		case "authentication":
			return AuthenticationFailure
		}
	}
	if err == nil {
		if status >= 200 && status < 400 {
			return Success
		}
		return Neutral
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		return TransportFailure
	}
	u, parseErr := url.Parse(proxyURL)
	if parseErr != nil || u.Hostname() == "" {
		return Neutral
	}
	// A dial error alone does not identify the failed endpoint. Match it to the
	// configured proxy; errors on reads and TLS to the target remain neutral.
	if op != nil && op.Op == "dial" && op.Addr != nil {
		port := u.Port()
		if port == "" {
			port = "80"
			if u.Scheme == "https" {
				port = "443"
			} else if strings.HasPrefix(u.Scheme, "socks") {
				port = "1080"
			}
		}
		if strings.EqualFold(op.Addr.String(), net.JoinHostPort(u.Hostname(), port)) {
			return TransportFailure
		}
	}
	var dns *net.DNSError
	if op != nil && op.Op == "dial" && errors.As(err, &dns) && strings.EqualFold(strings.TrimSuffix(dns.Name, "."), strings.TrimSuffix(u.Hostname(), ".")) {
		return TransportFailure
	}
	return Neutral
}

// Acquire never waits and never falls back to direct access when an enabled,
// configured pool has no eligible proxy. An unconfigured manager allows direct.
func (m *Manager) Acquire(ctx context.Context) (*Reference, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return &Reference{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquireLocked()
}

// AcquireURL acquires exactly this fixed proxy, without consulting or rotating
// the provider list. The transport must still apply its egress policy.
func (m *Manager) AcquireURL(ctx context.Context, address string) (*Reference, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if address == "" {
		return m.Acquire(ctx)
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, errors.New("invalid fixed proxy URL")
	}
	if m == nil {
		return &Reference{address: address}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	m.initLocked()
	if !m.health.Enabled {
		return &Reference{address: address}, nil
	}
	e := m.entries[address]
	if e == nil || !e.fixed {
		if !m.makeFixedRoomLocked() {
			return nil, ErrHealthCapacity
		}
	}
	if e == nil {
		e = newEntry(address)
		m.entries[address] = e
	}
	e.fixed = true
	return m.takeLocked(e)
}

func (m *Manager) NextChecked(ctx context.Context) (string, error) {
	r, err := m.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer r.Release()
	return r.URL(), nil
}

// WaitAcquire waits without holding the manager lock, respecting cancellation,
// cooldown expiry, list refresh, configuration changes and probe completion.
func (m *Manager) WaitAcquire(ctx context.Context) (*Reference, error) {
	if m == nil {
		return m.Acquire(ctx)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		r, err := m.acquireLocked()
		if !errors.Is(err, ErrUnavailable) {
			m.mu.Unlock()
			return r, err
		}
		changed := m.changed
		var delay time.Duration
		now := m.timeLocked()
		for _, e := range m.entries {
			if e.active && e.state == StateCooling && e.until.After(now) {
				if d := e.until.Sub(now); delay == 0 || d < delay {
					delay = d
				}
			}
		}
		m.mu.Unlock()
		var timer *time.Timer
		var tick <-chan time.Time
		if delay > 0 {
			timer = time.NewTimer(delay)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (m *Manager) acquireLocked() (*Reference, error) {
	if m.closed {
		return nil, ErrClosed
	}
	m.initLocked()
	if len(m.proxies) == 0 {
		if m.health.Enabled && m.apiURL != "" {
			return nil, ErrUnavailable
		}
		return &Reference{}, nil
	}
	start := m.index
	m.index++
	for i := range m.proxies {
		address := m.proxies[(start+uint64(i))%uint64(len(m.proxies))]
		e := m.entries[address]
		if !m.health.Enabled {
			return &Reference{address: address}, nil
		}
		if r, err := m.takeLocked(e); err == nil {
			return r, nil
		}
	}
	return nil, ErrUnavailable
}

func (m *Manager) takeLocked(e *healthEntry) (*Reference, error) {
	if e.state == StateDisabled {
		return nil, ErrUnavailable
	}
	now := m.timeLocked()
	if e.state == StateCooling {
		if now.Before(e.until) {
			return nil, ErrUnavailable
		}
		e.state = StateProbing
		e.revision++
	}
	probe := e.state == StateProbing
	if probe && m.probes >= m.health.MaxProbes {
		return nil, ErrUnavailable
	}
	e.inflight++
	e.lastUsed = now
	if probe {
		e.probes++
		m.probes++
	}
	return &Reference{manager: m, entry: e, address: e.address, version: e.version, probe: probe, tracked: m.health.Enabled}, nil
}

func (m *Manager) feedbackLocked(e *healthEntry, probe bool, kind FailureKind) {
	if kind != Success && kind != TransportFailure && kind != AuthenticationFailure {
		return
	}
	e.lastResult, e.lastUpdated = kind, m.timeLocked()
	e.revision++
	if kind == Success {
		e.failures = 0
		if probe {
			e.successes++
			if e.successes < m.health.RecoverySuccesses {
				return
			}
			e.version++ // Remaining probes belong to the completed recovery cycle.
		}
		e.state, e.cooldown, e.until = StateAvailable, 0, time.Time{}
		e.successes = 0
		return
	}
	e.failures++
	e.successes = 0
	if !probe && e.failures < m.health.FailureThreshold {
		return
	}
	if e.cooldown == 0 {
		e.cooldown = m.health.InitialCooldown
	} else if e.cooldown >= m.health.MaxCooldown/2 {
		e.cooldown = m.health.MaxCooldown
	} else {
		e.cooldown *= 2
	}
	if e.cooldown > m.health.MaxCooldown {
		e.cooldown = m.health.MaxCooldown
	}
	e.until = e.lastUpdated.Add(e.cooldown)
	e.state = StateCooling
	e.version++ // Old in-flight results cannot undo this cooldown.
}

func (m *Manager) Snapshot() []HealthSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	now := m.timeLocked()
	out := make([]HealthSnapshot, 0, len(m.entries))
	for _, e := range m.entries {
		remaining := e.until.Sub(now)
		if remaining < 0 || e.state != StateCooling {
			remaining = 0
		}
		out = append(out, HealthSnapshot{ID: e.id, State: e.state, Active: e.active, Fixed: e.fixed,
			ConsecutiveFailures: e.failures, RecoverySuccesses: e.successes, CooldownUntil: e.until,
			CooldownRemaining: remaining, InFlight: e.inflight, Probes: e.probes,
			LastUpdated: e.lastUpdated, LastResult: e.lastResult, Version: e.revision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Disable blocks new uses; existing requests continue with their original proxy.
func (m *Manager) Disable(id string) error { return m.setEnabled(id, false, nil) }

// Enable permits bounded probes, and does not claim the proxy is healthy.
func (m *Manager) Enable(id string) error { return m.setEnabled(id, true, nil) }

// DisableCAS and EnableCAS reject stale administrator snapshots atomically.
func (m *Manager) DisableCAS(id string, version uint64) error {
	return m.setEnabled(id, false, &version)
}

func (m *Manager) EnableCAS(id string, version uint64) error {
	return m.setEnabled(id, true, &version)
}

func (m *Manager) setEnabled(id string, enabled bool, version *uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if !m.health.Enabled {
		return ErrHealthDisabled
	}
	for _, e := range m.entries {
		if e.id != id {
			continue
		}
		if version != nil && e.revision != *version {
			return ErrConflict
		}
		state := StateDisabled
		if enabled {
			state = StateProbing
		}
		if e.state == state {
			return nil
		}
		e.state, e.until, e.successes = state, time.Time{}, 0
		e.lastUpdated = m.timeLocked()
		e.version++
		e.revision++
		m.signalLocked()
		return nil
	}
	return ErrUnknownProxy
}

// AcquireProbe returns a candidate for an explicit, caller-performed probe. It
// never opens a network connection or fabricates a result. Cooling and disabled
// proxies cannot be forced through; Enable makes a disabled proxy probe-eligible.
func (m *Manager) AcquireProbe(ctx context.Context, id string) (*Reference, error) {
	return m.acquireProbe(ctx, id, nil)
}

func (m *Manager) AcquireProbeCAS(ctx context.Context, id string, version uint64) (*Reference, error) {
	return m.acquireProbe(ctx, id, &version)
}

func (m *Manager) acquireProbe(ctx context.Context, id string, version *uint64) (*Reference, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if !m.health.Enabled {
		return nil, ErrHealthDisabled
	}
	for _, e := range m.entries {
		if e.id != id || (!e.active && !e.fixed) {
			continue
		}
		if version != nil && e.revision != *version {
			return nil, ErrConflict
		}
		if m.probes >= m.health.MaxProbes {
			return nil, ErrUnavailable
		}
		if e.state == StateAvailable {
			// Explicit verification participates in the same global probe budget,
			// without marking a currently healthy proxy unverified.
			r, err := m.takeLocked(e)
			if err == nil {
				r.probe = true
				e.probes++
				m.probes++
				e.revision++
			}
			return r, err
		}
		r, err := m.takeLocked(e)
		if err == nil {
			e.revision++
		}
		return r, err
	}
	return nil, ErrUnknownProxy
}

func opaqueID(address string) string {
	hash := sha256.Sum256([]byte(address))
	return "proxy_" + hex.EncodeToString(hash[:16])
}

func newEntry(address string) *healthEntry {
	return &healthEntry{id: opaqueID(address), address: address, state: StateAvailable, version: 1, revision: 1}
}

func (m *Manager) initLocked() {
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	if m.entries == nil {
		m.entries = make(map[string]*healthEntry)
		for _, address := range m.proxies {
			e := newEntry(address)
			e.active = true
			m.entries[address] = e
		}
	}
}

func (m *Manager) timeLocked() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) signalLocked() {
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}

func (m *Manager) replaceProxies(addresses []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.initLocked()
	for _, e := range m.entries {
		e.active = false
	}
	list := make([]string, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		list = append(list, address)
		e := m.entries[address]
		if e == nil {
			e = newEntry(address)
			m.entries[address] = e
		}
		e.active = true
	}
	m.proxies = list
	m.pruneLocked()
	m.signalLocked()
}

func (m *Manager) pruneLocked() {
	for address, e := range m.entries {
		if !e.active && !e.fixed && e.inflight == 0 {
			delete(m.entries, address)
		}
	}
}

func (m *Manager) makeFixedRoomLocked() bool {
	count := 0
	var oldest *healthEntry
	for _, e := range m.entries {
		if !e.fixed {
			continue
		}
		count++
		if !e.active && e.inflight == 0 && e.state == StateAvailable && (oldest == nil || e.lastUsed.Before(oldest.lastUsed)) {
			oldest = e
		}
	}
	if count < MaxFixedHealthEntries {
		return true
	}
	if oldest != nil {
		delete(m.entries, oldest.address)
		return true
	}
	return false
}

// String only emits the safe identifier when references are logged accidentally.
func (r *Reference) String() string { return fmt.Sprintf("proxy reference %s", r.ID()) }

func (r *Reference) GoString() string { return r.String() }
