package engine

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// BindAccountHealthEvents connects the pool's bounded asynchronous listener to
// the existing Notifier channel. Call after NewPools; nil removes the binding.
// No pool/controller lock is held while sending notifications. Stop unbinds it.
func (e *Engine) BindAccountHealthEvents(pools *account.Pools) {
	var unsubscribe func()
	if pools != nil {
		unsubscribe = pools.AddEventListener(e.NotifyAccountHealthEvent)
	}
	e.mu.Lock()
	old := e.accountEventUnsubscribe
	e.accountEventUnsubscribe = unsubscribe
	e.mu.Unlock()
	if old != nil {
		old()
	}
}

// NotifyAccountHealthEvent can also be used by a custom event integration. It
// is synchronous; bind through Pools.AddEventListener for bounded async delivery.
// Each kind/account/pool emits at most one notification per minute; the map is
// bounded to 2048 entries. Recovery is a separate kind and is never held back by
// a preceding failure notification. Durable health events remain queryable.
func (e *Engine) NotifyAccountHealthEvent(event account.HealthEvent) {
	level := core.AlertWarn
	switch event.Kind {
	case "recovered", "pool-recovered":
		level = core.AlertInfo
	case "pool-exhausted", "verification-wait-too-long", "verification-overdue", "manual-verification":
		level = core.AlertCritical
	case "needs-verification", "verification-unknown", "verification-invalid", "proxy-unavailable", "fixed-proxy-unavailable", "expired", "standby-shortage", "login-failed":
	default:
		return
	}
	site := event.Key.Site
	if site == "" {
		site = event.Pool.Site
	}
	key := site + "\x00" + event.Pool.Name + "\x00" + event.Key.ID + "\x00" + event.Kind
	now := time.Now()
	if !e.accountAlerts.allow(key, now) {
		return
	}
	reason := boundedCompensationReason(event.Reason)
	e.emit(events.Event{Type: "account_health", Site: site, TaskID: uint(event.TaskID), Kind: event.Kind, Source: "account", Attrs: map[string]string{"account_id": event.Key.ID, "account_pool": event.Pool.Name, "reason": reason}})
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	alert := core.AlertEvent{Level: level, AccountPool: event.Pool.Name, AccountID: event.Key.ID, TaskError: core.TaskError{Site: site, TaskID: int(event.TaskID), Kind: "account_" + event.Kind, Message: reason}}
	for _, n := range e.getNotifiers() {
		if err := n.Notify(ctx, alert); err != nil && e.loggerSet != nil {
			e.loggerSet.Engine.Warnf("account health notification failed: %s", err)
		}
	}
}
