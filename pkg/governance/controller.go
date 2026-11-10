package governance

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrRemoteWaitExceeded = errors.New("remote retry-after exceeds automatic wait budget; site paused")

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type Option func(*Controller)

func WithClock(clock Clock) Option {
	return func(c *Controller) {
		if clock != nil {
			c.clock = clock
		}
	}
}

type Class string

const (
	ClassTask    Class = "task"
	ClassBrowser Class = "browser"
)

type Feedback struct {
	Latency    time.Duration
	StatusCode int
	RetryAfter string
	Failed     bool
}

type siteState struct {
	inflight         int
	queue            []*requestWaiter
	readinessWaiting int
	tokens           float64
	tokenTime        time.Time
	lastStart        time.Time
	autoDelay        time.Duration
	cooldownUntil    time.Time
	paused           bool
	requests         uint64
	errors           uint64
	limited          uint64
	lastLatency      time.Duration
	lastAdjustment   time.Time
	lastReason       string
	lastService      time.Time
	firstRequest     time.Time
	rateBuckets      [60]rateBucket
}

type rateBucket struct {
	second int64
	count  uint64
}
type requestWaiter struct{ identity byte }
type permitWaiter struct {
	site, stage string
	physical    int
	class       Class
	ready       chan struct{}
	granted     bool
}
type stageKey struct{ site, stage string }
type resourceState struct {
	inflight int
	limit    int
	queue    map[string][]*permitWaiter
	current  map[string]int64
	bySite   map[string]int
}

// Controller owns all mutable state; accessors return independent snapshots.
type Controller struct {
	mu                 sync.Mutex
	clock              Clock
	changed            chan struct{}
	policyChanged      chan struct{}
	defaults           Policy
	policies           map[string]Policy
	resources          ResourcePolicy
	sites              map[string]*siteState
	classes            map[Class]*resourceState
	stages             map[stageKey]int
	metrics            Metrics
	highWindows        int
	lastResourceChange time.Time
	resourceReason     string
}

func New(config Config, opts ...Option) (*Controller, error) {
	c := &Controller{clock: realClock{}, changed: make(chan struct{}), policyChanged: make(chan struct{}), sites: make(map[string]*siteState), stages: make(map[stageKey]int), classes: make(map[Class]*resourceState)}
	for _, opt := range opts {
		opt(c)
	}
	for _, class := range []Class{ClassTask, ClassBrowser} {
		c.classes[class] = &resourceState{queue: make(map[string][]*permitWaiter), current: make(map[string]int64), bySite: make(map[string]int)}
	}
	if err := c.Update(config); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Update(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	defaults := MergePolicy(config.Defaults, PolicyPatch{})
	policies := make(map[string]Policy, len(config.Sites))
	for site := range config.Sites {
		policies[site] = config.Effective(site)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oldResources := c.resources
	now := c.clock.Now()
	for site, s := range c.sites {
		old := c.policyLocked(site).Request
		c.refillLocked(s, old, now)
		next := defaults.Request
		if p, ok := policies[site]; ok {
			next = p.Request
		}
		if next.RequestsPerSecond > 0 {
			if old.RequestsPerSecond <= 0 {
				s.tokens = float64(next.Burst)
			} else {
				s.tokens = math.Min(s.tokens, float64(next.Burst))
			}
		}
		if next.AutoThrottle.Enabled {
			if !old.AutoThrottle.Enabled {
				s.autoDelay = next.AutoThrottle.MinDelay
			} else {
				s.autoDelay = clampDuration(s.autoDelay, next.AutoThrottle.MinDelay, next.AutoThrottle.MaxDelay)
			}
		}
	}
	c.defaults, c.policies, c.resources = defaults, policies, config.Resources
	for class, r := range c.classes {
		maxLimit := c.maxLocked(class)
		if !oldResources.Enabled || !c.resources.Enabled {
			r.limit = maxLimit
		} else {
			r.limit = min(r.limit, maxLimit)
		}
	}
	c.highWindows = 0
	close(c.policyChanged)
	c.policyChanged = make(chan struct{})
	c.notifyLocked()
	c.dispatchLocked()
	return nil
}

func (c *Controller) policyLocked(site string) Policy {
	if p, ok := c.policies[site]; ok {
		return p
	}
	return c.defaults
}
func (c *Controller) Effective(site string) Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return MergePolicy(c.policyLocked(site), PolicyPatch{})
}

// PolicyChanges closes after the next successful Update. Consumers read the
// channel before Effective, then wait for it or their own lifecycle context.
// Request traffic and sampling do not wake policy observers.
func (c *Controller) PolicyChanges() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policyChanged
}
func (c *Controller) stateLocked(site string) *siteState {
	if s := c.sites[site]; s != nil {
		return s
	}
	p := c.policyLocked(site).Request
	s := &siteState{tokens: float64(p.Burst), tokenTime: c.clock.Now(), autoDelay: p.AutoThrottle.MinDelay}
	c.sites[site] = s
	return s
}
func (c *Controller) notifyLocked() { close(c.changed); c.changed = make(chan struct{}) }
func (c *Controller) refillLocked(s *siteState, p RequestPolicy, now time.Time) {
	if now.After(s.tokenTime) && p.RequestsPerSecond > 0 {
		s.tokens = math.Min(float64(p.Burst), s.tokens+now.Sub(s.tokenTime).Seconds()*p.RequestsPerSecond)
	}
	if !now.Before(s.tokenTime) {
		s.tokenTime = now
	}
}
func (c *Controller) delayLocked(s *siteState, p RequestPolicy) time.Duration {
	if !p.Enabled {
		return 0
	}
	if p.AutoThrottle.Enabled {
		return s.autoDelay
	}
	return p.ManualDelay
}

// WaitReady waits only for remote cooldown, before a caller acquires task capacity.
// Every actual exchange still needs AcquireRequest after this check.
func (c *Controller) WaitReady(ctx context.Context, site string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		s := c.stateLocked(site)
		ch := c.changed
		d := s.cooldownUntil.Sub(c.clock.Now())
		paused := s.paused
		c.mu.Unlock()
		if paused {
			return ErrRemoteWaitExceeded
		}
		if d <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-c.clock.After(d):
		}
	}
}

// WaitTaskReady parks unclaimed work during remote cooldown or a budget pause.
// Unlike explicit request admission it does not return an error while paused,
// avoiding a pending-task reclaim loop. AcknowledgeRemote only permits waiting
// for the remaining server deadline; cancellation always remains available.
func (c *Controller) WaitTaskReady(ctx context.Context, site string) error {
	counted := false
	defer func() {
		if counted {
			c.mu.Lock()
			c.stateLocked(site).readinessWaiting--
			c.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		s := c.stateLocked(site)
		ch := c.changed
		d := s.cooldownUntil.Sub(c.clock.Now())
		paused := s.paused
		if !counted && (paused || d > 0) {
			s.readinessWaiting++
			counted = true
		}
		c.mu.Unlock()
		if !paused && d <= 0 {
			return nil
		}
		var timer <-chan time.Time
		if !paused && d > 0 {
			timer = c.clock.After(d)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-timer:
		}
	}
}

// AcknowledgeRemote allows automatic waiting again, without shortening the
// server's original deadline. It does not bypass any request or resource limit.
func (c *Controller) AcknowledgeRemote(site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateLocked(site).paused = false
	c.notifyLocked()
	c.dispatchLocked()
}

// AcquireRequest shares a site's concurrency, token bucket, spacing and cooldown
// across all callers. Release is idempotent and may receive empty feedback when
// an observation has already been delivered at response-header time.
func (c *Controller) AcquireRequest(ctx context.Context, site string) (func(Feedback), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &requestWaiter{}
	c.mu.Lock()
	s := c.stateLocked(site)
	s.queue = append(s.queue, w)
	c.mu.Unlock()
	remove := func() { c.mu.Lock(); c.removeRequestLocked(s, w); c.notifyLocked(); c.mu.Unlock() }
	for {
		if err := ctx.Err(); err != nil {
			remove()
			return nil, err
		}
		c.mu.Lock()
		p := c.policyLocked(site).Request
		now := c.clock.Now()
		c.refillLocked(s, p, now)
		if s.paused {
			c.removeRequestLocked(s, w)
			c.notifyLocked()
			c.mu.Unlock()
			return nil, ErrRemoteWaitExceeded
		}
		d := max(time.Duration(0), s.cooldownUntil.Sub(now))
		if p.Enabled {
			if !s.lastStart.IsZero() {
				d = max(d, s.lastStart.Add(c.delayLocked(s, p)).Sub(now))
			}
			if p.RequestsPerSecond > 0 && s.tokens < 1 {
				d = max(d, secondsDuration((1-s.tokens)/p.RequestsPerSecond))
			}
		}
		if s.queue[0] == w && d <= 0 && (!p.Enabled || p.MaxInflight == 0 || s.inflight < p.MaxInflight) {
			s.queue = s.queue[1:]
			s.inflight++
			s.requests++
			if s.firstRequest.IsZero() {
				s.firstRequest = now
			}
			index := int((now.Unix()%60 + 60) % 60)
			if s.rateBuckets[index].second != now.Unix() {
				s.rateBuckets[index] = rateBucket{second: now.Unix()}
			}
			s.rateBuckets[index].count++
			s.lastStart = now
			if p.Enabled && p.RequestsPerSecond > 0 {
				s.tokens = math.Max(0, s.tokens-1)
			}
			c.notifyLocked()
			c.mu.Unlock()
			var once sync.Once
			return func(f Feedback) {
				once.Do(func() {
					c.mu.Lock()
					defer c.mu.Unlock()
					s.inflight--
					c.observeLocked(site, f)
					c.notifyLocked()
					c.dispatchLocked()
				})
			}, nil
		}
		ch := c.changed
		c.mu.Unlock()
		var timer <-chan time.Time
		if d > 0 {
			timer = c.clock.After(d)
		}
		select {
		case <-ctx.Done():
			remove()
			return nil, ctx.Err()
		case <-ch:
		case <-timer:
		}
	}
}

func (c *Controller) removeRequestLocked(s *siteState, w *requestWaiter) {
	for i, v := range s.queue {
		if v == w {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}
func secondsDuration(v float64) time.Duration {
	if v >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(math.Ceil(v * float64(time.Second)))
}
func clampDuration(v, lo, hi time.Duration) time.Duration { return max(lo, min(v, hi)) }

// RetryAfter parses delta-seconds and HTTP dates. Overflowing positive seconds
// saturate to the largest duration, so an excessive remote wait is never lost.
func RetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	digits := true
	for _, r := range value {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(time.Duration(0), deadline.Sub(now)), true
}

func (c *Controller) Observe(site string, f Feedback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observeLocked(site, f)
	c.notifyLocked()
	c.dispatchLocked()
}
func (c *Controller) observeLocked(site string, f Feedback) {
	if f.StatusCode == 0 && !f.Failed && f.Latency == 0 {
		return
	}
	s := c.stateLocked(site)
	p := c.policyLocked(site).Request
	now := c.clock.Now()
	failed := f.Failed || f.StatusCode >= 400
	if failed {
		s.errors++
	}
	if f.Latency >= 0 {
		s.lastLatency = f.Latency
	}
	if p.Enabled && (f.StatusCode == 429 || f.StatusCode == 503) {
		d, ok := RetryAfter(f.RetryAfter, now)
		if f.StatusCode == 429 && !ok {
			d, ok = p.RetryAfterDefault, true
		}
		if ok {
			s.limited++
			deadline := now.Add(d)
			if deadline.After(s.cooldownUntil) {
				s.cooldownUntil = deadline
			}
			if d > p.RetryAfterMax {
				s.paused = true
				s.lastReason = "remote_wait_exceeds_budget"
			} else {
				s.lastReason = "remote_cooldown"
			}
			s.lastAdjustment = now
		}
	}
	if p.Enabled && p.AutoThrottle.Enabled && f.Latency > 0 {
		a := p.AutoThrottle
		target := clampDuration(secondsDuration(f.Latency.Seconds()/a.TargetConcurrency), a.MinDelay, a.MaxDelay)
		next := time.Duration(float64(s.autoDelay)*(1-a.Smoothing) + float64(target)*a.Smoothing)
		next = clampDuration(next, a.MinDelay, a.MaxDelay)
		if (!failed || next > s.autoDelay) && next != s.autoDelay {
			s.autoDelay = next
			s.lastAdjustment = now
			s.lastReason = "response_latency"
		}
	}
}

func (c *Controller) maxLocked(class Class) int {
	if class == ClassBrowser {
		return c.resources.BrowserMax
	}
	return c.resources.TaskMax
}

func (c *Controller) Acquire(ctx context.Context, site string, class Class) (func(), error) {
	return c.acquire(ctx, site, "", 0, class)
}

// AcquireStage obtains stage and global task capacity atomically; a blocked
// stage cannot reserve global capacity needed by another ready site.
func (c *Controller) AcquireStage(ctx context.Context, site, stage string, physicalMax int) (func(), error) {
	if physicalMax <= 0 {
		return nil, errors.New("physical stage capacity must be positive")
	}
	return c.acquire(ctx, site, stage, physicalMax, ClassTask)
}
func (c *Controller) StageDelay(site, stage string, fallback time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.policyLocked(site).Stages[stage]; ok {
		return s.Delay
	}
	return fallback
}
func (c *Controller) stageReadyLocked(w *permitWaiter) bool {
	if w.stage == "" {
		return true
	}
	limit := w.physical
	if configured := c.policyLocked(w.site).Stages[w.stage].MaxConcurrent; configured > 0 {
		limit = min(limit, configured)
	}
	return c.stages[stageKey{w.site, w.stage}] < limit
}
func (c *Controller) acquire(ctx context.Context, site, stage string, physical int, class Class) (func(), error) {
	if class != ClassTask && class != ClassBrowser {
		return nil, errors.New("unknown resource class")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &permitWaiter{site: site, stage: stage, physical: physical, class: class, ready: make(chan struct{})}
	c.mu.Lock()
	r := c.classes[class]
	r.queue[site] = append(r.queue[site], w)
	c.stateLocked(site)
	c.dispatchLocked()
	c.mu.Unlock()
	for {
		c.mu.Lock()
		s := c.stateLocked(site)
		d := s.cooldownUntil.Sub(c.clock.Now())
		ch := c.changed
		paused := s.paused
		c.mu.Unlock()
		var timer <-chan time.Time
		if d > 0 {
			timer = c.clock.After(d)
		}
		if ctx.Err() != nil || paused {
			c.cancelPermit(w)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrRemoteWaitExceeded
		}
		select {
		case <-ctx.Done():
			c.cancelPermit(w)
			return nil, ctx.Err()
		case <-w.ready:
			if err := ctx.Err(); err != nil {
				c.cancelPermit(w)
				return nil, err
			}
			var once sync.Once
			return func() {
				once.Do(func() { c.mu.Lock(); c.releasePermitLocked(w); c.dispatchLocked(); c.notifyLocked(); c.mu.Unlock() })
			}, nil
		case <-ch:
		case <-timer:
			c.mu.Lock()
			c.dispatchLocked()
			c.mu.Unlock()
		}
	}
}
func (c *Controller) releasePermitLocked(w *permitWaiter) {
	r := c.classes[w.class]
	r.inflight--
	r.bySite[w.site]--
	if w.stage != "" {
		c.stages[stageKey{w.site, w.stage}]--
	}
}
func (c *Controller) cancelPermit(w *permitWaiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.classes[w.class]
	if w.granted {
		c.releasePermitLocked(w)
	} else {
		for i, q := range r.queue[w.site] {
			if q == w {
				r.queue[w.site] = append(r.queue[w.site][:i], r.queue[w.site][i+1:]...)
				break
			}
		}
	}
	c.dispatchLocked()
	c.notifyLocked()
}

func (c *Controller) dispatchLocked() {
	now := c.clock.Now()
	for _, class := range []Class{ClassTask, ClassBrowser} {
		r := c.classes[class]
		for !c.resources.Enabled || r.inflight < r.limit {
			// Stable sorted iteration plus smooth weighted round-robin gives every
			// ready site service even with a single global execution slot.
			sites := make([]string, 0, len(r.queue))
			candidates := make(map[string]int)
			for site, queue := range r.queue {
				s := c.stateLocked(site)
				if s.paused || s.cooldownUntil.After(now) {
					continue
				}
				for i, w := range queue {
					if c.stageReadyLocked(w) {
						sites = append(sites, site)
						candidates[site] = i
						break
					}
				}
			}
			if len(sites) == 0 {
				break
			}
			sort.Strings(sites)
			chosen := ""
			total := int64(0)
			for _, site := range sites {
				p := c.policyLocked(site)
				weight := int64(max(1, max(p.Weight, p.MinWeight)))
				r.current[site] += weight
				total += weight
				if chosen == "" || r.current[site] > r.current[chosen] {
					chosen = site
				}
			}
			r.current[chosen] -= total
			idx := candidates[chosen]
			queue := r.queue[chosen]
			w := queue[idx]
			r.queue[chosen] = append(queue[:idx], queue[idx+1:]...)
			r.inflight++
			r.bySite[chosen]++
			w.granted = true
			if w.stage != "" {
				c.stages[stageKey{w.site, w.stage}]++
			}
			c.stateLocked(chosen).lastService = now
			close(w.ready)
		}
	}
}
