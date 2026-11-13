// Package reportrunner owns a bounded, signal-only auxiliary reporting service.
package reportrunner

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

type Config struct {
	Enabled             bool
	ReportDir           string
	OnExit              bool
	Interval, Retention time.Duration
}
type Options struct {
	Config         Config
	RunID, Version string
	Inputs         obs.Inputs
}

// Runner owns intake admission, one collector and one writer. It never borrows
// business resources. A timed-out writer cannot be restarted on this instance.
type Runner struct {
	options                                 Options
	mu                                      sync.RWMutex
	started, closed                         bool
	incoming                                chan envelope
	requests                                chan obs.Trigger
	stop, done                              chan struct{}
	jobs                                    chan *obs.Report
	groups                                  int
	pruning                                 retentionCursor // writer-owned directory cursor
	dropped, truncated, failures, coalesced atomic.Uint64
	write                                   func(string, *obs.Report) ([]string, error)
}
type envelope struct {
	event    events.Event
	received time.Time
}

func New(o Options) *Runner { return NewWithLimits(o, inputbudget.Limits{}) }

// NewWithLimits uses MaxEvents for intake slots, MaxDedupKeys for dimensions,
// and MaxRetainedBytes for a conservative 64KiB-per-slot admission budget.
// Offline file/scan/spool budgets do not apply to this signal-only service.
func NewWithLimits(o Options, l inputbudget.Limits) *Runner {
	l = l.Normalize()
	n := min(1024, l.MaxEvents)
	n = min(n, int(max(int64(1), l.MaxRetainedBytes/(64<<10))))
	g := min(256, l.MaxDedupKeys)
	g = min(g, int(max(int64(1), l.MaxRetainedBytes/1024)))
	o.RunID = bounded(o.RunID)
	o.Version = bounded(o.Version)
	// Copy only finite provenance. Threshold callbacks/maps are not online inputs.
	in := o.Inputs
	o.Inputs = obs.Inputs{PapaVersion: bounded(in.PapaVersion), PapaCommit: bounded(in.PapaCommit), ConfigFile: bounded(in.ConfigFile), ThresholdsSource: bounded(in.ThresholdsSource)}
	o.Inputs.Thresholds = make(map[string]string)
	for k, v := range in.Thresholds {
		if len(o.Inputs.Thresholds) >= 32 {
			break
		}
		o.Inputs.Thresholds[bounded(k)] = bounded(v)
	}
	return &Runner{options: o, incoming: make(chan envelope, n), requests: make(chan obs.Trigger, 1), jobs: make(chan *obs.Report, 1), stop: make(chan struct{}), done: make(chan struct{}), groups: g, write: report.Write}
}
func bounded(s string) string {
	if len(s) > 256 {
		// Prefix truncation can remove credential delimiters before rendering
		// performs redaction. Omit without scanning arbitrary input instead.
		return "[omitted: report string budget]"
	}
	return strings.Clone(s)
}
func (r *Runner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.closed || !r.options.Config.Enabled {
		return
	}
	r.started = true
	go r.writer()
	go r.collect()
}

// Emit never waits for queue capacity or for report processing.
func (r *Runner) Emit(ev events.Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.started || r.closed {
		return
	}
	ev, _ = events.Detach(ev)
	if ev.Truncated {
		r.truncated.Add(1)
	}
	select {
	case r.incoming <- envelope{ev, time.Now()}:
	default:
		r.dropped.Add(1)
	}
}
func (r *Runner) Generate(t obs.Trigger) {
	r.Start()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.started || r.closed {
		return
	}
	select {
	case r.requests <- t:
	default:
	}
}
func (r *Runner) Periodic() { r.Generate(obs.TriggerPeriodic) }
func (r *Runner) Exit() {
	if r.options.Config.OnExit {
		r.Generate(obs.TriggerExit)
	}
}
func (r *Runner) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.Close(ctx)
}

// Close shares the caller's deadline; no offline timeout can expand that budget.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.stop)
		if !r.started {
			close(r.done)
		}
	}
	r.mu.Unlock()
	select {
	case <-r.done:
		return nil
	default:
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status is an atomic diagnostic snapshot. Counters are monotonic but need not
// represent one transaction; it remains readable when report file I/O is stuck.
type Status struct {
	Dropped          uint64 `json:"dropped"`
	Truncated        uint64 `json:"truncated"`
	Failures         uint64 `json:"failures"`
	CoalescedReports uint64 `json:"coalesced_reports"`
}

func (r *Runner) Status() Status {
	return Status{Dropped: r.dropped.Load(), Truncated: r.truncated.Load(), Failures: r.failures.Load(), CoalescedReports: r.coalesced.Load()}
}
