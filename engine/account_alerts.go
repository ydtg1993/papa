package engine

import (
	"sync"
	"time"
)

// accountAlertGate owns bounded notification throttling, not durable account health.
type accountAlertGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (g *accountAlertGate) allow(key string, now time.Time) bool {
	g.mu.Lock()
	if last := g.last[key]; !last.IsZero() && now.Sub(last) < time.Minute {
		g.mu.Unlock()
		return false
	}
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	if len(g.last) >= 2048 {
		var oldestKey string
		var oldest time.Time
		for k, at := range g.last {
			if oldest.IsZero() || at.Before(oldest) {
				oldestKey, oldest = k, at
			}
		}
		delete(g.last, oldestKey)
	}
	g.last[key] = now
	g.mu.Unlock()
	return true
}
