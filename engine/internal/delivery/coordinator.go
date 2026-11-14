// Package delivery owns in-process task admission and recovery notifications.
// Durable task contents belong to the store; no overflow task list lives here.
package delivery

import (
	"context"
	"sync"
)

type stage struct {
	wake     chan struct{}
	cursor   uint
	revision uint64
	end      bool
}
type owner struct {
	stage    string
	observed uint64
}
type Coordinator struct {
	mu      sync.Mutex
	owners  map[int]owner
	recheck map[int]string
	stages  map[string]*stage
	closed  bool
	active  int
	idle    chan struct{}
}

func New() *Coordinator {
	ch := make(chan struct{})
	close(ch)
	return &Coordinator{owners: map[int]owner{}, recheck: map[int]string{}, stages: map[string]*stage{}, idle: ch}
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
	s := c.stage(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, owned := c.owners[id]
	if c.closed || owned {
		if !c.closed && key != "" && s.revision > previous.observed {
			c.recheck[id] = key
		}
		return false
	}
	c.owners[id] = owner{stage: key, observed: s.revision}
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
	s := c.stage(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, owned := c.owners[id]
	if !owned {
		return false
	}
	if s.revision > previous.observed {
		c.recheck[id] = key
	}
	return true
}
func (c *Coordinator) Owned(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.owners[id]
	return ok
}

// ObservedWaiting records the generation inspected by a claim that found no
// eligible work. Events after that inspection still rearm delivery on release.
func (c *Coordinator) ObservedWaiting(id int, key string, revision uint64) {
	s := c.stage(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.owners[id]; !ok {
		return
	}
	c.owners[id] = owner{stage: key, observed: revision}
	if s.revision <= revision {
		delete(c.recheck, id)
	} else {
		c.recheck[id] = key
	}
}
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
	c.Restart(key)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Restart invalidates a scan generation without enqueuing another wake. Timers
// use it when they are already executing the requested scan.
func (c *Coordinator) Restart(key string) {
	s := c.stage(key)
	c.mu.Lock()
	s.cursor, s.end = 0, false
	s.revision++
	c.mu.Unlock()
}

// Continue signals released capacity without reconsidering earlier waiting
// rows. Only a durable state change (Wake) starts a new scan generation.
func (c *Coordinator) Continue(key string) {
	s := c.stage(key)
	c.mu.Lock()
	end := s.end
	c.mu.Unlock()
	if end {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ContinueAll broadcasts shared resident capacity to unfinished scans. It does
// not restart exhausted scans or change their durable-event revisions.
func (c *Coordinator) ContinueAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.stages {
		if !s.end {
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (c *Coordinator) Scan(key string) (uint, uint64, bool) {
	s := c.stage(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.cursor, s.revision, s.end
}

// Advance cannot overwrite a newer business event's reset cursor.
func (c *Coordinator) Advance(key string, revision uint64, cursor uint, end bool) bool {
	s := c.stage(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.revision != revision {
		return false
	}
	s.cursor, s.end = cursor, end
	return true
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
