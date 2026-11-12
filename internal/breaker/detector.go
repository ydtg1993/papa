package breaker

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Outcome is a terminal task round, never an individual request or attempt.
// Epoch changes only after full-site recovery or process reinitialization.
// Round must identify a persisted execution generation.
type Outcome struct {
	TaskID      int64     `json:"task_id"`
	Round       string    `json:"round"`
	Site        string    `json:"site"`
	Stage       string    `json:"stage"`
	URL         string    `json:"url"`
	At          time.Time `json:"at"`
	Success     bool      `json:"success"`
	Category    string    `json:"category"`
	Fingerprint string    `json:"fingerprint"`
	Error       string    `json:"error,omitempty"` // bounded, redacted by the engine before observation
	Kind        string    `json:"kind,omitempty"`
	Epoch       uint64    `json:"epoch"`
}

type Group struct {
	Stage       string `json:"stage"`
	Category    string `json:"category"`
	Fingerprint string `json:"fingerprint"`
	Count       int    `json:"count"`
	Tasks       int    `json:"tasks"`
}

type Decision struct {
	Accepted      bool    `json:"-"`     // this observation was new in the current epoch
	Rule          string  `json:"rule"`  // fast, mixed, hard, capacity; empty means normal
	Stage         string  `json:"stage"` // empty means site
	Failures      int     `json:"failures"`
	Successes     int     `json:"successes"`
	ShortFailures int     `json:"short_failures"`
	DistinctTasks int     `json:"distinct_tasks"`
	Groups        []Group `json:"groups"`
	Limited       bool    `json:"limited"`
	Epoch         uint64  `json:"epoch"`
}

// Detector uses exact bounded task-round records for failures. Capacity
// exhaustion protects the site instead of evicting failures and undercounting.
// Successful rounds use a separate bounded sample ring; Limited exposes loss of
// success detail and successes never subtract failures.
type Detector struct {
	mu             sync.Mutex
	cfg            Config
	policy         Policy
	epoch          uint64
	failures       []Outcome
	successes      []Outcome
	successLimited bool
	last           time.Time
	overflowUntil  time.Time
	anonymous      uint64
	legacyFailures int
	legacyUntil    time.Time
}

func NewDetector(cfg Config) *Detector {
	if err := Validate(cfg); err != nil {
		panic(err)
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	return &Detector{cfg: cfg, policy: EffectivePolicy(cfg.Policy)}
}

func (d *Detector) ResetEpoch(epoch uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch = epoch
	d.failures = nil
	d.successes = nil
	d.successLimited = false
	d.last = time.Time{}
	d.overflowUntil = time.Time{}
	d.legacyFailures, d.legacyUntil = 0, time.Time{}
}

func taskID(o Outcome) string {
	if o.TaskID != 0 {
		return fmt.Sprintf("%s\x00%d", o.Site, o.TaskID)
	}
	return o.Site + "\x00" + o.Stage + "\x00" + o.URL
}
func roundID(o Outcome) string { return taskID(o) + "\x00" + o.Round }

func (d *Detector) Observe(o Outcome) Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	if o.At.IsZero() {
		o.At = time.Now()
	}
	if !d.cfg.Enabled || o.Epoch != d.epoch {
		return d.snapshot(d.last)
	}
	if o.At.After(d.last) {
		d.last = o.At
	}
	d.prune(d.last)
	// Direct non-persistent callers have no reliable task identity to dedup.
	if o.TaskID == 0 && o.Round == "" {
		d.anonymous++
		o.TaskID = -int64(d.anonymous)
	}
	if !o.At.After(d.last.Add(-max(d.cfg.Window, d.policy.ShortWindow))) {
		return d.snapshot(d.last)
	}
	key := roundID(o)
	for _, old := range d.failures {
		if roundID(old) == key {
			return d.snapshot(d.last)
		}
	}
	for _, old := range d.successes {
		if roundID(old) == key {
			return d.snapshot(d.last)
		}
	}
	if o.Success {
		if len(d.successes) == d.policy.Capacity {
			d.successes = d.successes[1:]
			d.successLimited = true
		}
		d.successes = append(d.successes, o)
	} else if len(d.failures) >= d.policy.Capacity {
		d.overflowUntil = d.last.Add(max(d.cfg.Window, d.policy.ShortWindow))
	} else {
		d.failures = append(d.failures, o)
	}
	out := d.snapshot(d.last)
	out.Accepted = true
	return out
}

func (d *Detector) Snapshot(now time.Time) Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	if now.Before(d.last) {
		now = d.last
	}
	d.last = now
	d.prune(now)
	return d.snapshot(now)
}

// Evidence projects the same records used for counting; no second sample cache.
// The caller holds its outcome/admission lock when combining this with Snapshot.
func (d *Detector) Evidence(now time.Time, limit int) ([]Outcome, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		return nil, len(d.failures) > 0
	}
	cutoff := now.Add(-d.cfg.Window)
	var out []Outcome
	for _, o := range d.failures {
		if o.At.After(cutoff) {
			out = append(out, o)
		}
	}
	limited := len(out) > limit
	if limited {
		out = out[len(out)-limit:]
	}
	return out, limited
}

func (d *Detector) prune(now time.Time) {
	cutoff := now.Add(-max(d.cfg.Window, d.policy.ShortWindow))
	keep := func(rows []Outcome) []Outcome {
		n := 0
		for _, o := range rows {
			if o.At.After(cutoff) {
				rows[n] = o
				n++
			}
		}
		clear(rows[n:])
		return rows[:n]
	}
	d.failures = keep(d.failures)
	d.successes = keep(d.successes)
	if len(d.successes) == 0 {
		d.successLimited = false
	}
}

func (d *Detector) snapshot(now time.Time) Decision {
	out := Decision{Epoch: d.epoch, Limited: d.successLimited, Groups: []Group{}}
	if now.Before(d.legacyUntil) {
		out.Failures = d.legacyFailures
	}
	if !d.cfg.Enabled {
		return out
	}
	short, regular := now.Add(-d.policy.ShortWindow), now.Add(-d.cfg.Window)
	type aggregate struct {
		g     Group
		tasks map[string]bool
	}
	groups := map[string]*aggregate{}
	distinct := map[string]bool{}
	for _, o := range d.failures {
		if o.At.After(regular) {
			out.Failures++
		}
		if !o.At.After(short) {
			continue
		}
		out.ShortFailures++
		distinct[taskID(o)] = true
		k := o.Stage + "\x00" + o.Category + "\x00" + o.Fingerprint
		a := groups[k]
		if a == nil {
			a = &aggregate{g: Group{Stage: o.Stage, Category: o.Category, Fingerprint: o.Fingerprint}, tasks: map[string]bool{}}
			groups[k] = a
		}
		a.g.Count++
		a.tasks[taskID(o)] = true
	}
	for _, o := range d.successes {
		if o.At.After(regular) {
			out.Successes++
		}
	}
	out.DistinctTasks = len(distinct)
	for _, a := range groups {
		a.g.Tasks = len(a.tasks)
		out.Groups = append(out.Groups, a.g)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := out.Groups[i], out.Groups[j]
		if a.Tasks != b.Tasks {
			return a.Tasks > b.Tasks
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Fingerprint < b.Fingerprint
	})
	if now.Before(d.overflowUntil) {
		out.Rule = "capacity"
		out.Limited = true
		return out
	}
	if out.Failures >= d.cfg.Threshold {
		out.Rule = "hard"
		return out
	}
	fastStages := map[string]bool{}
	for _, g := range out.Groups {
		if g.Tasks >= d.policy.FastThreshold {
			out.Rule = "fast"
			out.Stage = g.Stage
			fastStages[g.Stage] = true
		}
	}
	if out.Rule == "fast" {
		if len(fastStages) > 1 {
			out.Stage = ""
		}
		return out
	}
	if out.DistinctTasks >= d.policy.MixedThreshold {
		out.Rule = "mixed"
	}
	return out
}
