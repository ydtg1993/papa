package account

import (
	"context"
	"errors"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

// checkTransport deliberately never invokes Resume, ensure or WithBrowser.
// It probes a detached copy of the exact credential generation being reviewed.
func (p *Pools) checkTransport(ctx context.Context, key Key, t Transport, expected, browserVersion uint64) (version, bv uint64, err error) {
	defer func() {
		if recover() != nil {
			err = &Failure{Code: "verification-panic"}
		}
	}()
	ctx, release, err := p.manager.PinIdentity(ctx, key)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	e, err := p.manager.get(key)
	if err != nil {
		return 0, 0, err
	}
	if err = e.enter(ctx); err != nil {
		return 0, 0, err
	}
	defer e.leave()
	if err = e.load(ctx); err != nil {
		return 0, 0, err
	}
	if err = e.validateIdentityState(ctx); err != nil {
		return 0, 0, err
	}
	e.mu.Lock()
	version, bv = e.version, e.browserRevision
	if (expected != 0 && expected != version) || (browserVersion != 0 && (t == Browser || t == Both) && browserVersion != bv) {
		e.mu.Unlock()
		return version, bv, ErrSuperseded
	}
	c := cloneCredentials(e.current)
	state, err := clonePageState(e.pageState)
	e.mu.Unlock()
	if err != nil {
		return version, bv, err
	}
	if t == HTTP || t == Both {
		if c == nil || e.config.Check == nil {
			return version, bv, ErrExpired
		}
		c.Session, err = htmlfetch.RestoreSession(c.Session.Snapshot())
		if err != nil {
			return version, bv, err
		}
		ctx, err = e.context(ctx)
		if err != nil {
			return version, bv, err
		}
		valid, checkErr := e.config.Check(ctx, c)
		if checkErr != nil {
			status, _ := htmlfetch.StatusCode(checkErr)
			kind := proxy.ClassifyHTTP(e.config.ProxyURL, status, checkErr)
			if kind == proxy.TransportFailure || kind == proxy.AuthenticationFailure || errors.Is(checkErr, proxy.ErrUnavailable) {
				return version, bv, &Failure{Code: "fixed-proxy-unavailable", Cause: errors.Join(ErrFixedProxyUnavailable, checkErr)}
			}
			return version, bv, &Failure{Code: "existing-check-failed", Cause: checkErr}
		}
		if !valid {
			return version, bv, ErrExpired
		}
	}
	if t == Browser || t == Both {
		if state == nil || e.config.Browser == nil {
			return version, bv, ErrExpired
		}
		if err = e.enterBrowser(ctx); err != nil {
			return version, bv, err
		}
		defer func() { <-e.browserSlot }()
		err = useBrowser(ctx, e.config.Browser, state, func(page *rod.Page) error {
			valid, err := e.config.Browser.Check(ctx, page)
			if err != nil {
				return &Failure{Code: "existing-browser-check-failed", Cause: err}
			}
			if err = browserOrigin(page, e.config.Browser.URL); err != nil {
				return err
			}
			if !valid {
				return ErrExpired
			}
			return nil
		})
	}
	return version, bv, err
}

func (p *Pools) finishVerification(key Key, t Transport, before ManagementState, mode VerificationMode, v, bv uint64, err error, cooldown time.Duration) {
	p.mu.Lock()
	a := p.accounts[key]
	a.verifying = false
	a.lastVerify = time.Now()
	if a.state.Generation != before.Generation || a.state.Disabled || p.ctx.Err() != nil {
		p.publishLocked()
		p.mu.Unlock()
		return
	}
	e, getErr := p.manager.get(key)
	if getErr != nil {
		err = getErr
	}
	if e != nil {
		e.mu.Lock()
		if mode == CheckExisting && (e.version != v || ((t == Browser || t == Both) && e.browserRevision != bv)) {
			err = ErrSuperseded
		}
		if errors.Is(err, ErrSuperseded) {
			// New generations must themselves be checked; stale results provide no evidence.
			a.state.CredentialVersion = e.version
			a.state.BrowserVersion = e.browserRevision
			a.state.InvalidChecks = 0
		}
		if err == nil && mode == CheckExisting {
			if t == HTTP || t == Both {
				e.lastCheck = time.Now()
			}
			if t == Browser || t == Both {
				e.browserCheck = time.Now()
			}
			e.failure = nil
			e.retryAt = time.Time{}
		}
		e.mu.Unlock()
	}
	now := time.Now()
	a.state.advanceControl()
	a.state.Generation++
	a.state.UpdatedAt = now
	a.dirty = true
	// Evidence of an invalid session does not diagnose a failed proxy. A proxy
	// probe must succeed before credentials are evaluated in a later round.
	switch {
	case err == nil:
		a.state.Quarantined = false
		a.state.Suspicious = false
		a.state.ProxyUnavailable = false
		a.state.PendingInvalidation = false
		a.state.VerificationPending = false
		a.state.InvalidChecks = 0
		a.state.UnknownChecks = 0
		a.state.ProxyChecks = 0
		a.state.NextVerifyAt = time.Time{}
		a.state.CooldownUntil = time.Time{}
		a.state.Reason = ""
		a.state.LastVerifiedAt = now
		a.state.LastVerification = string(mode) + "-valid"
		p.eventLocked(a, "recovered", a.state.LastVerification)
	case errors.Is(err, ErrChallenge):
		a.state.Quarantined = true
		a.state.Suspicious = false
		a.state.VerificationPending = false
		a.state.Reason = "manual-verification"
		a.state.LastVerification = "manual-verification"
		p.eventLocked(a, "manual-verification", a.state.Reason)
	case errors.Is(err, ErrFixedProxyUnavailable):
		a.state.ProxyUnavailable = true
		a.state.ProxyChecks++
		a.state.LastVerification = "proxy-unavailable"
		a.state.Reason = "fixed-proxy-unavailable"
		a.state.UnknownChecks = 0
		if a.state.ProxyChecks >= a.state.Policy.InvalidConfirmations {
			a.state.Quarantined = true
			a.state.Suspicious = false
			a.state.VerificationPending = false
			a.verify = false
			a.state.CooldownUntil = now.Add(cooldown)
		} else {
			a.state.VerificationPending = true
			a.verify = true
			a.state.NextVerifyAt = now.Add(a.state.Policy.CheckInterval)
		}
		p.eventLocked(a, "proxy-unavailable", a.state.Reason)
	case errors.Is(err, ErrExpired):
		a.state.ProxyUnavailable = false
		a.state.ProxyChecks = 0
		a.state.InvalidChecks++
		a.state.LastVerification = "invalid"
		a.state.UnknownChecks = 0
		if mode == RecoverLogin || a.state.InvalidChecks >= a.state.Policy.InvalidConfirmations {
			a.state.Quarantined = true
			a.state.Suspicious = false
			a.state.VerificationPending = false
			a.state.Reason = "expired"
			a.state.CooldownUntil = now.Add(cooldown)
			// Keep the other transport's credentials. Quarantine itself is durable.
			if e != nil {
				e.mu.Lock()
				if e.version == v && mode == CheckExisting {
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
				e.mu.Unlock()
			}
			p.eventLocked(a, "verification-invalid", "confirmed-invalid")
		} else {
			a.state.Suspicious = true
			a.state.VerificationPending = true
			a.state.NextVerifyAt = now.Add(a.state.Policy.CheckInterval)
			a.verify = true
			p.eventLocked(a, "verification-invalid", "confirmation-pending")
		}
	default:
		a.state.UnknownChecks++
		a.state.LastVerification = "unknown"
		a.state.VerificationPending = true
		a.verify = true
		if mode == CheckExisting && !a.state.ProxyUnavailable {
			a.state.Suspicious = true
		}
		backoff := a.state.Policy.UnknownBackoff
		for n := 1; n < a.state.UnknownChecks && backoff < a.state.Policy.MaxBackoff; n++ {
			backoff *= 2
			if backoff > a.state.Policy.MaxBackoff {
				backoff = a.state.Policy.MaxBackoff
			}
		}
		a.state.NextVerifyAt = now.Add(backoff)
		a.state.Reason = "verification-unknown"
		if before.ProxyUnavailable || errors.Is(err, ErrFixedProxyUnavailable) {
			a.state.ProxyUnavailable = true
			a.state.Reason = "fixed-proxy-unavailable"
		}
		p.eventLocked(a, "verification-unknown", a.state.Reason)
		if !a.state.SuspectSince.IsZero() && now.Sub(a.state.SuspectSince) >= a.state.Policy.UnknownAlertAfter {
			p.eventLocked(a, "verification-overdue", "verification-wait-budget-exceeded")
		}
	}
	p.wakeAccountLocked(key)
	p.publishLocked()
	p.mu.Unlock()
	if p.writes.LockContext(p.ctx) == nil {
		_ = p.flushManagement(p.ctx, key)
		p.writes.Unlock()
	}
}

func (p *Pools) preverifyStandbys(s PoolState) {
	if s.StandbyTarget <= 0 {
		return
	}
	readyCount := 0
	var candidates []Key
	p.mu.Lock()
	for _, id := range s.Members {
		if id == s.CurrentID {
			continue
		}
		key := Key{Site: s.Key.Site, ID: id}
		a := p.accounts[key]
		if a == nil || a.state.Disabled || a.state.Quarantined || a.state.Suspicious || a.state.ProxyUnavailable {
			continue
		}
		if p.memberLocked(key, s.Transport).State == "available" && time.Since(a.state.LastVerifiedAt) < s.StandbyTTL {
			readyCount++
		} else {
			candidates = append(candidates, key)
		}
	}
	if q := p.pools[s.Key]; q != nil {
		if readyCount < s.StandbyTarget && !q.standbyNotified {
			if a := p.accounts[Key{Site: s.Key.Site, ID: s.CurrentID}]; a != nil {
				a.state.Version++
				a.dirty = true
				p.eventForPoolLocked(a, s.Key, "standby-shortage", "standby-target-not-met")
			}
			q.standbyNotified = true
		} else if readyCount >= s.StandbyTarget {
			q.standbyNotified = false
		}
	}
	p.mu.Unlock()
	for _, key := range candidates {
		if readyCount >= s.StandbyTarget {
			return
		}
		p.verifyAccount(key, s.Transport, 1, s.Cooldown, false)
		readyCount++
	}
}
