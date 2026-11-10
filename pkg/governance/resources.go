package governance

import (
	"context"
	"math"
	"sort"
	"time"
)

// Metrics must describe one consistent resource scope. Valid=false means
// unknown, not zero load; it prevents automatic recovery.
type Metrics struct {
	CPUPercent           float64   `json:"cpu_percent"`
	MemoryPercent        float64   `json:"memory_percent"`
	AvailableMemoryBytes uint64    `json:"available_memory_bytes"`
	Valid                bool      `json:"valid"`
	Source               string    `json:"source"`
	SampledAt            time.Time `json:"sampled_at"`
}
type Sampler interface {
	Sample(context.Context) (Metrics, error)
}

// RunSampler samples independently of observers or the admin page. The caller
// owns its lifecycle, normally by starting it with the engine's context.
func (c *Controller) RunSampler(ctx context.Context, sampler Sampler) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := sampler.Sample(ctx)
		if err != nil {
			m.Valid = false
		}
		c.Sample(m)
		c.mu.Lock()
		interval := c.resources.SampleInterval
		c.mu.Unlock()
		if interval <= 0 {
			interval = 2 * time.Second
		}
		// Runtime notifications are intentionally excluded here: request events
		// must not increase OS sampling frequency.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.clock.After(interval):
		}
	}
}

func (c *Controller) Sample(m Metrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	p := c.resources
	if m.SampledAt.IsZero() {
		m.SampledAt = now
	}
	if !finite(m.CPUPercent) || !finite(m.MemoryPercent) || m.CPUPercent < 0 || m.CPUPercent > 100 || m.MemoryPercent < 0 || m.MemoryPercent > 100 || m.SampledAt.After(now) || (!c.metrics.SampledAt.IsZero() && !m.SampledAt.After(c.metrics.SampledAt)) || sampleStale(now, m.SampledAt, p.SampleInterval) {
		m.Valid = false
	}
	// Invalid float values cannot be allowed to break snapshot JSON encoding.
	if !finite(m.CPUPercent) {
		m.CPUPercent = 0
	}
	if !finite(m.MemoryPercent) {
		m.MemoryPercent = 0
	}
	if len(m.Source) > 128 {
		m.Source = m.Source[:128]
	}
	c.metrics = m
	if !p.Enabled {
		return
	}
	if !m.Valid {
		c.highWindows = 0
		c.resourceReason = "sample_unavailable"
		c.notifyLocked()
		return
	}
	if m.CPUPercent >= p.CPUHigh {
		c.highWindows++
	} else {
		c.highWindows = 0
	}
	memoryHigh := m.MemoryPercent >= p.MemoryHigh || m.AvailableMemoryBytes < p.MinimumAvailableBytes
	cpuHigh := c.highWindows >= p.HighWindows
	if memoryHigh || cpuHigh {
		if cpuHigh {
			c.classes[ClassTask].limit = shrink(c.classes[ClassTask].limit, p.ShrinkFactor)
			c.highWindows = 0
		}
		c.classes[ClassBrowser].limit = shrink(c.classes[ClassBrowser].limit, p.ShrinkFactor)
		c.lastResourceChange = now
		c.resourceReason = "cpu_pressure"
		if memoryHigh {
			c.resourceReason = "memory_pressure"
		}
	} else if m.CPUPercent <= p.CPURecover && m.MemoryPercent <= p.MemoryRecover && m.AvailableMemoryBytes >= p.MinimumAvailableBytes && (c.lastResourceChange.IsZero() || now.Sub(c.lastResourceChange) >= p.RecoveryInterval) {
		changed := false
		for class, r := range c.classes {
			old := r.limit
			r.limit += min(c.maxLocked(class)-r.limit, p.RecoveryStep)
			changed = changed || old != r.limit
		}
		if changed {
			c.lastResourceChange = now
			c.resourceReason = "healthy_recovery"
		}
	}
	c.dispatchLocked()
	c.notifyLocked()
}
func shrink(n int, factor float64) int { return max(1, int(math.Floor(float64(n)*factor))) }
func sampleStale(now, at time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	age := now.Sub(at)
	return age > interval && age-interval > interval
}

type SiteSnapshot struct {
	Site              string        `json:"site"`
	Policy            Policy        `json:"policy"`
	Inflight          int           `json:"inflight"`
	Waiting           int           `json:"waiting"`
	ReadinessWaiting  int           `json:"readiness_waiting"`
	EffectiveDelay    time.Duration `json:"effective_delay"`
	CooldownUntil     time.Time     `json:"cooldown_until"`
	RemotePaused      bool          `json:"remote_paused"`
	Requests          uint64        `json:"requests"`
	RequestsPerSecond float64       `json:"requests_per_second"`
	RateWindow        time.Duration `json:"rate_window"`
	RateSampled       bool          `json:"rate_sampled"`
	Errors            uint64        `json:"errors"`
	RateLimited       uint64        `json:"rate_limited"`
	LastLatency       time.Duration `json:"last_latency"`
	LastAdjustment    time.Time     `json:"last_adjustment"`
	LastReason        string        `json:"last_reason"`
	LastService       time.Time     `json:"last_service"`
	TaskInflight      int           `json:"task_inflight"`
	BrowserInflight   int           `json:"browser_inflight"`
	TaskWaiting       int           `json:"task_waiting"`
	BrowserWaiting    int           `json:"browser_waiting"`
}
type PermitSnapshot struct {
	Limit    int `json:"limit"`
	Inflight int `json:"inflight"`
	Waiting  int `json:"waiting"`
}
type Snapshot struct {
	At                 time.Time      `json:"at"`
	Sites              []SiteSnapshot `json:"sites"`
	Resources          ResourcePolicy `json:"resources"`
	Task               PermitSnapshot `json:"task"`
	Browser            PermitSnapshot `json:"browser"`
	Metrics            Metrics        `json:"metrics"`
	SampleValid        bool           `json:"sample_valid"`
	LastResourceChange time.Time      `json:"last_resource_change"`
	ResourceReason     string         `json:"resource_reason"`
}

// Snapshot is read-only: it neither creates site states nor triggers sampling.
// Every returned map is copied, so clients cannot mutate active policy.
func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	out := Snapshot{At: now, Resources: c.resources, Metrics: c.metrics, LastResourceChange: c.lastResourceChange, ResourceReason: c.resourceReason, Sites: []SiteSnapshot{}}
	out.SampleValid = c.metrics.Valid && !c.metrics.SampledAt.IsZero() && !c.metrics.SampledAt.After(now) && !sampleStale(now, c.metrics.SampledAt, c.resources.SampleInterval)
	for class, r := range c.classes {
		p := PermitSnapshot{Limit: r.limit, Inflight: r.inflight}
		for _, q := range r.queue {
			p.Waiting += len(q)
		}
		if !c.resources.Enabled {
			p.Limit = 0
		}
		if class == ClassTask {
			out.Task = p
		} else {
			out.Browser = p
		}
	}
	keys := make(map[string]bool, len(c.sites)+len(c.policies))
	for k := range c.sites {
		keys[k] = true
	}
	for k := range c.policies {
		keys[k] = true
	}
	sites := make([]string, 0, len(keys))
	for k := range keys {
		sites = append(sites, k)
	}
	sort.Strings(sites)
	for _, site := range sites {
		p := c.policyLocked(site)
		v := SiteSnapshot{Site: site, Policy: MergePolicy(p, PolicyPatch{}), TaskInflight: c.classes[ClassTask].bySite[site], BrowserInflight: c.classes[ClassBrowser].bySite[site], TaskWaiting: len(c.classes[ClassTask].queue[site]), BrowserWaiting: len(c.classes[ClassBrowser].queue[site])}
		if s := c.sites[site]; s != nil {
			v.Inflight = s.inflight
			v.Waiting = len(s.queue)
			v.ReadinessWaiting = s.readinessWaiting
			v.EffectiveDelay = c.delayLocked(s, p.Request)
			v.CooldownUntil = s.cooldownUntil
			v.RemotePaused = s.paused
			v.Requests = s.requests
			if !s.firstRequest.IsZero() && now.Sub(s.firstRequest) >= time.Second {
				v.RateSampled = true
				v.RateWindow = min(time.Minute, now.Sub(s.firstRequest))
				var count uint64
				for _, bucket := range s.rateBuckets {
					if bucket.second > now.Unix()-60 && bucket.second <= now.Unix() {
						count += bucket.count
					}
				}
				v.RequestsPerSecond = float64(count) / v.RateWindow.Seconds()
			}
			v.Errors = s.errors
			v.RateLimited = s.limited
			v.LastLatency = s.lastLatency
			v.LastAdjustment = s.lastAdjustment
			v.LastReason = s.lastReason
			v.LastService = s.lastService
		} else if p.Request.Enabled {
			v.EffectiveDelay = p.Request.ManualDelay
			if p.Request.AutoThrottle.Enabled {
				v.EffectiveDelay = p.Request.AutoThrottle.MinDelay
			}
		}
		out.Sites = append(out.Sites, v)
	}
	return out
}
