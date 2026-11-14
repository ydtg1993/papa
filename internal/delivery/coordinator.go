// Package delivery owns in-process task admission and recovery notifications.
// Durable task contents belong to the store; no overflow task list lives here.
package delivery

import (
	"context"
	"sync"
)

type stage struct{ wake chan struct{} }
type Coordinator struct {
	mu      sync.Mutex
	owners  map[int]bool
	recheck map[int]string
	stages  map[string]*stage
	closed  bool
	active  int
	idle    chan struct{}
}

func New() *Coordinator {
	ch := make(chan struct{})
	close(ch)
	return &Coordinator{owners: map[int]bool{}, recheck: map[int]string{}, stages: map[string]*stage{}, idle: ch}
}
func (c *Coordinator) Begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.active++
	return true
}
func (c *Coordinator) End() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	if c.active == 0 {
		close(c.idle)
	}
}
func (c *Coordinator) Close() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.idle
}
func (c *Coordinator) Acquire(id int) bool {
	return c.AcquireForStage(id, "")
}

// AcquireForStage also rearms a skipped admission after the existing owner exits.
func (c *Coordinator) AcquireForStage(id int, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.owners[id] {
		if !c.closed && key != "" {
			c.recheck[id] = key
		}
		return false
	}
	c.owners[id] = true
	return true
}
func (c *Coordinator) Release(id int) {
	c.mu.Lock()
	delete(c.owners, id)
	key, recheck := c.recheck[id]
	delete(c.recheck, id)
	c.mu.Unlock()
	if recheck {
		c.Wake(key)
	}
}

// SkipOwned atomically records that a durable pending row was skipped. Release
// must rearm recovery: a terminal task may have been reset while its previous
// worker was still finishing callbacks or sleeping between tasks.
func (c *Coordinator) SkipOwned(id int, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owners[id] {
		return false
	}
	c.recheck[id] = key
	return true
}
func (c *Coordinator) Owned(id int) bool { c.mu.Lock(); defer c.mu.Unlock(); return c.owners[id] }
func (c *Coordinator) stage(key string) *stage {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stages[key]
	if s == nil {
		s = &stage{wake: make(chan struct{}, 1)}
		c.stages[key] = s
	}
	return s
}
func (c *Coordinator) Wake(key string) {
	s := c.stage(key)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Merge cancels work when either the caller or the engine is stopped.
func Merge(caller, lifecycle context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(caller)
	stop := context.AfterFunc(lifecycle, cancel)
	if lifecycle.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

// Notifications exposes receive-only wakeups; only the coordinator can signal them.
func (c *Coordinator) Notifications(key string) <-chan struct{} { return c.stage(key).wake }
