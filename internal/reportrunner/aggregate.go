package reportrunner

import (
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"sort"
	"time"
)

type counters struct {
	Received uint64 `json:"received"`
	Finished uint64 `json:"task_finished_signals"`
	Failed   uint64 `json:"task_failed_signals"`
	Retry    uint64 `json:"retry_scheduled_signals"`
}
type groupKey struct{ Site, Stage string }
type group struct {
	Site   string   `json:"site"`
	Stage  string   `json:"stage"`
	Counts counters `json:"counts"`
}
type failureSample struct {
	Site, Stage, Error string
	At                 time.Time
}
type aggregate struct {
	day                                                string
	from, last                                         time.Time
	counts, run                                        counters
	groups                                             map[groupKey]counters
	overflow                                           uint64
	late                                               uint64 // run-wide signals excluded from an already advanced day
	stop                                               *events.Event
	stages                                             map[string]events.StageSnapshot
	queues                                             map[string]events.QueueSnapshot
	samples                                            []failureSample
	upstreamDropped, upstreamTruncated, archiveDropped uint64
}

func add(c *counters, e events.Event) {
	c.Received++
	switch e.Type {
	case events.TypeTaskFinished:
		c.Finished++
	case events.TypeTaskFailed:
		c.Failed++
	case events.TypeTaskRetry:
		c.Retry++
	}
}

// advanceDay never rolls a bucket backward when concurrent admission or a
// midnight snapshot overtakes older queued signals. Only the collector owns it.
func (a *aggregate) advanceDay(at time.Time) {
	day := report.DayName(at)
	if a.day < day {
		a.day = day
		a.from = at
		a.counts = counters{}
		a.groups = make(map[groupKey]counters)
		a.overflow = 0
		a.stages = make(map[string]events.StageSnapshot)
		a.queues = make(map[string]events.QueueSnapshot)
		a.samples = nil
	}
}

func (r *Runner) accept(a *aggregate, e envelope) {
	a.advanceDay(e.received)
	if a.last.Before(e.received) {
		a.last = e.received
	}
	add(&a.run, e.event)
	if e.event.Type == "observation_loss" {
		a.upstreamDropped += uint64(max(0, e.event.Counts["dropped"]))
		a.upstreamTruncated += uint64(max(0, e.event.Counts["truncated"]))
		a.archiveDropped += uint64(max(0, e.event.Counts["archive_dropped"]))
	}
	if e.event.Type == events.TypeRunStopped {
		v := e.event
		a.stop = &v
	}
	if report.DayName(e.received) < a.day {
		a.late++
		return
	}
	if e.received.Before(a.from) {
		a.from = e.received
	}
	for k, v := range e.event.Stages {
		if _, ok := a.stages[k]; ok || len(a.stages) < r.groups {
			a.stages[k] = v
		} else {
			a.overflow++
		}
	}
	for k, v := range e.event.Queues {
		if _, ok := a.queues[k]; ok || len(a.queues) < r.groups {
			a.queues[k] = v
		} else {
			a.overflow++
		}
	}
	if e.event.Type == events.TypeTaskFailed {
		if !e.event.Truncated && (len(e.event.Site) > 256 || len(e.event.Stage) > 256 || len(e.event.Error) > 256) {
			r.truncated.Add(1)
		}
		sample := failureSample{bounded(e.event.Site), bounded(e.event.Stage), bounded(e.event.Error), e.received}
		if len(a.samples) == 16 {
			copy(a.samples, a.samples[1:])
			a.samples = a.samples[:15]
		}
		a.samples = append(a.samples, sample)
	}
	add(&a.counts, e.event)
	// A shortened identifier is not an identity: never silently combine two
	// sites or stages sharing a prefix. Prior transport truncation is uncertain.
	if e.event.Truncated || len(e.event.Site) > 256 || len(e.event.Stage) > 256 {
		a.overflow++
		return
	}
	k := groupKey{e.event.Site, e.event.Stage}
	if _, ok := a.groups[k]; ok || len(a.groups) < r.groups {
		c := a.groups[k]
		add(&c, e.event)
		a.groups[k] = c
	} else {
		a.overflow++
	}
}
func (r *Runner) collect() {
	defer func() {
		if recover() != nil {
			r.failures.Add(1)
		}
	}()
	a := aggregate{}
	var tick <-chan time.Time
	if r.options.Config.Interval > 0 {
		t := time.NewTicker(r.options.Config.Interval)
		defer t.Stop()
		tick = t.C
	}
	defer close(r.jobs)
	for {
		select {
		case e := <-r.incoming:
			r.accept(&a, e)
		case <-r.requests:
			r.enqueue(r.snapshot(&a, time.Now()))
		case <-tick:
			r.enqueue(r.snapshot(&a, time.Now()))
		case <-r.stop:
			for {
				select {
				case e := <-r.incoming:
					r.accept(&a, e)
				default:
					if r.options.Config.OnExit {
						r.enqueue(r.snapshot(&a, time.Now()))
					}
					return
				}
			}
		}
	}
}
func (r *Runner) enqueue(rep *obs.Report) {
	select {
	case r.jobs <- rep:
		return
	default:
	}
	select {
	case <-r.jobs:
		r.coalesced.Add(1)
		rep.Sections[0].Metrics.(map[string]any)["coalesced_reports"] = r.coalesced.Load()
	default:
	}
	select {
	case r.jobs <- rep:
	default:
	}
}
func (r *Runner) snapshot(a *aggregate, now time.Time) *obs.Report {
	a.advanceDay(now)
	gs := make([]group, 0, len(a.groups))
	for k, c := range a.groups {
		gs = append(gs, group{k.Site, k.Stage, c})
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Site != gs[j].Site {
			return gs[i].Site < gs[j].Site
		}
		return gs[i].Stage < gs[j].Stage
	})
	stages := make(map[string]events.StageSnapshot, len(a.stages))
	for k, v := range a.stages {
		stages[k] = v
	}
	queues := make(map[string]events.QueueSnapshot, len(a.queues))
	for k, v := range a.queues {
		queues[k] = v
	}
	samples := append([]failureSample(nil), a.samples...)
	inputs := r.options.Inputs
	inputs.SnapshotAt = time.Time{}
	inputs.WindowFrom = a.from
	inputs.WindowTo = now
	reason := "仅基于本进程收到的部分信号，不代表数据库权威任务总数；未查询数据库、未读取日志、未重放历史。"
	if a.counts.Received == 0 {
		reason = "当日尚未收到信号，执行状态未知。"
	}
	rep := &obs.Report{SchemaVersion: obs.SchemaVersion, GeneratedAt: now, Verdict: obs.VerdictUnknown, VerdictReason: reason, Mode: obs.ModeSnapshot, Run: obs.RunMeta{From: a.from, To: now}, EventMeta: obs.EventMeta{Count: int(a.counts.Received)}, Inputs: inputs, Engine: obs.EngineState{Source: "signals", CapturedAt: a.last, Degraded: "partial：未获取权威引擎快照"}, Environment: map[string]string{"papa_version": r.options.Version, "source": "signals"}, Degraded: []string{reason}}
	if r.options.RunID != "" {
		rep.Run.RunIDs = []string{r.options.RunID}
	}
	rep.Sections = []obs.SectionReport{{Key: "signals", Title: "在线观测信号", Summary: reason, Tables: signalTables(a), Metrics: map[string]any{"day": a.counts, "last_stage_gauges": stages, "last_queue_gauges": queues, "recent_failure_samples": samples, "upstream_dropped": a.upstreamDropped, "archive_dropped": a.archiveDropped, "upstream_truncated": a.upstreamTruncated, "coalesced_reports": r.coalesced.Load(), "run": a.run, "groups": gs, "group_overflow": a.overflow, "late_day_signals": a.late, "dropped": r.dropped.Load(), "truncated": r.truncated.Load(), "writer_failures": r.failures.Load(), "last_run_stop": a.stop, "coverage": "partial", "window_basis": "按接收自然日统计，仅覆盖当前进程"}}}
	if a.counts.Failed > 0 {
		rep.Problems = append(rep.Problems, obs.Problem{ID: "signals.failure", Severity: obs.SeverityWarn, Title: "收到任务失败信号", Count: int(a.counts.Failed), Scope: "仅统计信号，不等同于最终失败任务数", Origin: "section"})
	}
	if a.counts.Retry > 0 {
		rep.Problems = append(rep.Problems, obs.Problem{ID: "signals.retry", Severity: obs.SeverityWarn, Title: "收到重试调度信号", Count: int(a.counts.Retry), Scope: "未据此推断任务终态", Origin: "section"})
	}
	if r.dropped.Load()+r.truncated.Load()+a.upstreamDropped+a.upstreamTruncated+a.overflow+a.late > 0 {
		rep.Problems = append(rep.Problems, obs.Problem{ID: "signals.loss", Severity: obs.SeverityWarn, Title: "观测信号丢失、截断或维度超限", Scope: "数据不完整，计数不能作为完整验收结论", Origin: "section"})
	}
	if a.archiveDropped > 0 {
		rep.Problems = append(rep.Problems, obs.Problem{ID: "archive.loss", Severity: obs.SeverityWarn, Title: "事件归档存在丢失", Scope: "仅影响归档完整性，不计入在线信号入口丢失", Origin: "section"})
	}
	return rep
}
