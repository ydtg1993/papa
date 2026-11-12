package account

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"time"

	"github.com/go-rod/rod"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func (m *Manager) Version(key Key) (uint64, error) {
	e, err := m.get(key)
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.version, nil
}

func detachedCandidate(c *Credentials) (*Credentials, error) {
	if c == nil || c.Session == nil {
		return nil, errors.New("candidate session missing")
	}
	session, err := htmlfetch.RestoreSession(c.Session.Snapshot())
	if err != nil {
		return nil, &Failure{Code: "invalid-candidate-cookies", Cause: err}
	}
	state, err := clonePageState(c.BrowserState)
	if err != nil {
		return nil, &Failure{Code: "invalid-candidate-browser", Cause: err}
	}
	return &Credentials{Session: session, Headers: maps.Clone(c.Headers), Values: maps.Clone(c.Values), BrowserState: state}, nil
}

func verifyCandidate(ctx context.Context, e *entry, c *Credentials) (err error) {
	defer func() {
		if recover() != nil {
			err = &Failure{Code: "candidate-verification-panic"}
		}
	}()
	if err = e.checkCredentials(c); err != nil {
		return &Failure{Code: "invalid-candidate", Cause: err}
	}
	if e.config.Check != nil {
		probe, err := e.context(ctx)
		if err != nil {
			return err
		}
		valid, err := e.config.Check(probe, c)
		if err != nil {
			return &Failure{Code: "candidate-check-failed", Cause: err}
		}
		if !valid {
			return ErrExpired
		}
	}
	if c.BrowserState != nil {
		if e.config.Browser == nil {
			return ErrUnsupported
		}
		if err = useBrowser(ctx, e.config.Browser, c.BrowserState, func(page *rod.Page) error {
			valid, err := e.config.Browser.Check(ctx, page)
			if err != nil {
				return &Failure{Code: "candidate-browser-check-failed", Cause: err}
			}
			if !valid {
				return ErrExpired
			}
			return browserOrigin(page, e.config.Browser.URL)
		}); err != nil {
			return err
		}
	} else if e.config.Check == nil {
		return ErrUnsupported
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return e.checkCredentials(c)
}

func saveCandidate(ctx context.Context, e *entry) (err error) {
	defer func() {
		if recover() != nil {
			err = &Failure{Code: "candidate-save-panic"}
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	return e.saveLocked(ctx)
}

func candidateTransport(e *entry, c *Credentials) Transport {
	if e.config.Check != nil {
		if c.BrowserState != nil {
			return Both
		}
		return HTTP
	}
	return Browser
}

// RegisterCandidate checks and durably saves a detached candidate before making
// the account visible. A failed check never leaves a harvestable registration.
func (m *Manager) RegisterCandidate(ctx context.Context, key Key, cfg Config, candidate *Credentials) error {
	ctx, release, err := m.PinIdentity(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	temp := NewManager(m.client)
	if err := temp.Register(key, cfg); err != nil {
		return err
	}
	e, _ := temp.get(key)
	c, err := detachedCandidate(candidate)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	if err = verifyCandidate(ctx, e, c); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; exists {
		return ErrPoolConflict
	}
	e.manager = m
	e.current = c
	e.seed = maps.Clone(c.Values)
	e.pageState = c.BrowserState
	e.version = 1
	e.revision = 1
	e.loaded = true
	e.lastLogin = time.Now()
	if e.config.Check != nil {
		e.lastCheck = e.lastLogin
	} else {
		e.current = nil
	}
	if scope, ok := identityruntime.FromContext(ctx); ok {
		e.identityState = identityMetadata(scope.Snapshot())
	}
	if c.BrowserState != nil {
		e.browserCheck = e.lastLogin
		e.browserRevision = 1
	}
	if err = saveCandidate(ctx, e); err != nil {
		return err
	}
	e.lastCandidateVersion = e.version
	e.lastCandidateTransport = candidateTransport(e, c)
	m.entries[key] = e
	return nil
}

// PublishCandidate checks without Login, then CAS-saves and publishes. Failure
// leaves existing credentials, browser state and pause state unchanged.
func (m *Manager) PublishCandidate(ctx context.Context, key Key, expectedVersion uint64, candidate *Credentials) (uint64, error) {
	ctx, release, err := m.PinIdentity(ctx, key)
	if err != nil {
		return 0, err
	}
	defer release()
	e, err := m.get(key)
	if err != nil {
		return 0, err
	}
	c, err := detachedCandidate(candidate)
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	revision, initialBrowserRevision := e.revision, e.browserRevision
	if e.version != expectedVersion {
		e.mu.Unlock()
		return 0, ErrSuperseded
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	if err = verifyCandidate(ctx, e, c); err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.version != expectedVersion || e.revision != revision || (c.BrowserState != nil && e.browserRevision != initialBrowserRevision) {
		return 0, ErrSuperseded
	}
	old, seed, page, version, rev, browserRev := e.current, e.seed, e.pageState, e.version, e.revision, e.browserRevision
	lastCheck, lastLogin, browserCheck, paused, failure, retry := e.lastCheck, e.lastLogin, e.browserCheck, e.paused, e.failure, e.retryAt
	loaded := e.loaded
	oldIdentity := cloneIdentityState(e.identityState)
	published := false
	defer func() {
		if !published {
			e.current = old
			e.seed = seed
			e.pageState = page
			e.version = version
			e.revision = rev
			e.browserRevision = browserRev
			e.lastCheck = lastCheck
			e.lastLogin = lastLogin
			e.browserCheck = browserCheck
			e.paused = paused
			e.failure = failure
			e.retryAt = retry
			e.loaded = loaded
			e.identityState = oldIdentity
		}
	}()
	e.current = c
	e.seed = maps.Clone(c.Values)
	e.version++
	e.revision++
	e.lastCheck = time.Now()
	e.lastLogin = e.lastCheck
	if e.config.Check == nil {
		e.current = nil
		e.lastCheck = time.Time{}
	}
	e.paused = false
	e.loaded = true
	e.failure = nil
	e.retryAt = time.Time{}
	if c.BrowserState != nil {
		e.pageState = c.BrowserState
		e.browserRevision++
		e.browserCheck = e.lastLogin
	}
	if scope, ok := identityruntime.FromContext(ctx); ok && scope.Snapshot() != nil {
		metadata := identityMetadata(scope.Snapshot())
		if c.BrowserState == nil && !reflect.DeepEqual(metadata, e.identityState) {
			e.pageState = nil
			e.browserCheck = time.Time{}
			e.browserRevision++
		}
		e.identityState = metadata
	}
	if err = saveCandidate(ctx, e); err != nil {
		e.current = old
		e.seed = seed
		e.pageState = page
		e.version = version
		e.revision = rev
		e.browserRevision = browserRev
		e.lastCheck = lastCheck
		e.lastLogin = lastLogin
		e.browserCheck = browserCheck
		e.paused = paused
		e.failure = failure
		e.retryAt = retry
		e.loaded = loaded
		return 0, err
	}
	published = true
	e.lastCandidateVersion = e.version
	e.lastCandidateTransport = candidateTransport(e, c)
	return e.version, nil
}

// CandidatePublished records health recovery after a separately committed
// candidate. Disabled remains an independent operator decision. If this save
// fails, the credential publication remains committed and health stays pending.
func (p *Pools) CandidatePublished(ctx context.Context, key Key, version uint64) error {
	if err := p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	e, err := p.manager.get(key)
	if err != nil {
		return err
	}
	p.mu.Lock()
	a := p.ensureManagement(key)
	e.mu.Lock()
	if e.version != version || e.lastCandidateVersion != version || e.paused || (e.lastCheck.IsZero() && e.browserCheck.IsZero()) || ((a.state.Suspicious || a.state.Quarantined) && version <= a.state.CredentialVersion) {
		e.mu.Unlock()
		p.mu.Unlock()
		return ErrSuperseded
	}
	if (a.state.CheckTransport == HTTP && e.lastCandidateTransport == Browser) || (a.state.CheckTransport == Browser && e.lastCandidateTransport == HTTP) || (a.state.CheckTransport == Both && e.lastCandidateTransport != Both) {
		e.mu.Unlock()
		p.mu.Unlock()
		return ErrUnsupported
	}
	a.state.CredentialVersion = version
	a.state.BrowserVersion = e.browserRevision
	e.mu.Unlock()
	advanceControl(&a.state)
	a.state.Generation++
	a.state.Suspicious = false
	a.state.Quarantined = false
	a.state.ProxyUnavailable = false
	a.state.PendingInvalidation = false
	a.state.VerificationPending = false
	a.verify = false
	a.state.CooldownUntil = time.Time{}
	a.state.NextVerifyAt = time.Time{}
	a.state.InvalidChecks = 0
	a.state.UnknownChecks = 0
	a.state.LastVerifiedAt = time.Now()
	a.state.UpdatedAt = time.Now()
	a.state.LastVerification = "candidate-published"
	a.state.Reason = ""
	if a.state.Disabled {
		a.state.Reason = "disabled"
	}
	a.dirty = true
	p.eventLocked(a, "recovered", "candidate-published")
	p.wakeAccountLocked(key)
	p.publishLocked()
	p.mu.Unlock()
	p.signal()
	return p.flushManagement(ctx, key)
}
