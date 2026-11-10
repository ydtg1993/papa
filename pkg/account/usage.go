package account

import (
	"context"
	"time"
)

type coordinatorContextKey struct{}

func usageReason(s ManagementState, now time.Time) string {
	v := s.Policy
	loc, err := time.LoadLocation(v.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	minute := local.Hour()*60 + local.Minute()
	if v.WorkStartMinute != v.WorkEndMinute {
		inside := minute >= v.WorkStartMinute && minute < v.WorkEndMinute
		if v.WorkStartMinute > v.WorkEndMinute {
			inside = minute >= v.WorkStartMinute || minute < v.WorkEndMinute
		}
		if !inside {
			return "outside-work-hours"
		}
	}
	if v.DailyLimit > 0 && s.Usage.Day == local.Format("2006-01-02") && s.Usage.Operations >= v.DailyLimit {
		return "daily-budget-exhausted"
	}
	if v.OperationInterval > 0 && now.Before(s.Usage.LastOperation.Add(v.OperationInterval)) {
		return "operation-interval"
	}
	return ""
}

func (m *Manager) beforeOperation(ctx context.Context, key Key) error {
	if ctx.Value(coordinatorContextKey{}) != nil {
		return nil
	}
	p := m.Pools()
	if p == nil {
		return nil
	}
	if err := p.guardAccount(key); err != nil {
		return err
	}
	if err := p.consume(ctx, key, false); err != nil {
		return err
	}
	return p.guardAccount(key)
}

// ReserveLogin enforces the shared minimum login interval before an external
// adapter begins a login. Failed attempts count too, preventing login storms.
func (m *Manager) ReserveLogin(ctx context.Context, key Key) error {
	p := m.Pools()
	if p == nil {
		return nil
	}
	return p.consume(ctx, key, true)
}

func (p *Pools) consume(ctx context.Context, key Key, login bool) error {
	if p.ctx.Err() != nil {
		return ErrPoolClosed
	}
	// Default policies do not add a serialized database write to each request.
	// Work hours alone only gate admission; usage counters are explicitly opt-in.
	p.mu.Lock()
	existing := p.accounts[key]
	if existing == nil {
		p.mu.Unlock()
		return nil
	}
	policy := existing.state.Policy
	if login && policy.LoginInterval == 0 {
		p.mu.Unlock()
		return nil
	}
	if !login && !policy.TrackUsage && policy.DailyLimit == 0 && policy.OperationInterval == 0 {
		reason := usageReason(existing.state, time.Now())
		p.mu.Unlock()
		if reason != "" {
			return ErrBudget
		}
		return nil
	}
	p.mu.Unlock()
	if err := p.writes.LockContext(ctx); err != nil {
		return err
	}
	defer p.writes.Unlock()
	p.mu.Lock()
	a := p.accounts[key]
	if a == nil {
		p.mu.Unlock()
		return nil
	}
	now := time.Now()
	if login {
		if a.state.Policy.LoginInterval > 0 && now.Before(a.state.Usage.LastLoginAttempt.Add(a.state.Policy.LoginInterval)) {
			p.mu.Unlock()
			return ErrBudget
		}
	} else {
		if a.state.Disabled {
			p.mu.Unlock()
			return ErrPaused
		}
		if a.state.Suspicious {
			p.mu.Unlock()
			return ErrNeedsVerify
		}
		if a.state.ProxyUnavailable {
			p.mu.Unlock()
			return ErrFixedProxyUnavailable
		}
		if a.state.Quarantined {
			p.mu.Unlock()
			return ErrExpired
		}
		if usageReason(a.state, now) != "" {
			p.mu.Unlock()
			return ErrBudget
		}
	}
	// Preserve an unsaved health exclusion: a usage reservation must not conceal it.
	if a.dirty {
		p.mu.Unlock()
		if err := p.flushManagement(ctx, key); err != nil {
			return err
		}
		p.mu.Lock()
		a = p.accounts[key]
		if !login {
			if a.state.Disabled {
				p.mu.Unlock()
				return ErrPaused
			}
			if a.state.Suspicious {
				p.mu.Unlock()
				return ErrNeedsVerify
			}
			if a.state.ProxyUnavailable {
				p.mu.Unlock()
				return ErrFixedProxyUnavailable
			}
			if a.state.Quarantined {
				p.mu.Unlock()
				return ErrExpired
			}
		}
	}
	if login {
		a.state.Usage.LastLoginAttempt = now
	} else {
		loc, _ := time.LoadLocation(a.state.Policy.Timezone)
		if loc == nil {
			loc = time.UTC
		}
		day := now.In(loc).Format("2006-01-02")
		if a.state.Usage.Day != day {
			a.state.Usage.Day = day
			a.state.Usage.Operations = 0
		}
		a.state.Usage.Operations++
		a.state.Usage.LastOperation = now
	}
	a.state.Usage.Unit = "operation"
	a.state.Version++
	a.state.UpdatedAt = now
	a.dirty = true
	p.publishLocked()
	p.mu.Unlock()
	return p.flushManagement(ctx, key)
}
