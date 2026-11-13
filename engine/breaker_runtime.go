package engine

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/core"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/pkg/incident"
)

// BreakerProbe must perform a bounded, read-only health check, never a FetchHandler
// or business write. Healthy and Semantic must both be true to reopen admission:
// an HTTP 200 alone does not prove that the failing page operation works again.
type BreakerProbe func(context.Context, BreakerProbeRequest) (BreakerProbeResult, error)
type BreakerProbeRequest struct {
	Site, Stage, Category string
	Evidence              []incident.Sample
	Pass                  int    // 1-based independent check within this stage's recovery round
	Coverage              string // declared complete read-only coverage; empty for ordinary probes
	SampleID              string // selected failed task identity; echo in result after checking this sample
	Sample                *incident.Sample
}
type BreakerProbeResult struct {
	Healthy, Semantic bool
	Complete          bool // this call fully checked the declared read-only scope, not HTTP status alone
	Detail            string
	SampleID          string
}

type breakerRepository interface {
	Save(context.Context, breakerstore.Snapshot) error
	LoadRuntime(context.Context) ([]breakerstore.Snapshot, error)
	List(context.Context, string, int) ([]incident.Lifecycle, error)
}
type smartBreaker struct {
	cfg               breaker.Config
	policy            breaker.Policy
	detector          *breaker.Detector
	epoch             uint64
	event             *breakerEvent
	dirty             bool
	controlRevision   uint64 // revision that must be saved before releasing protection
	savedRevision     uint64
	draining          bool // stop new admissions while settling the current recovery cohort
	inflight          int
	nextAdmission     time.Time
	probeRunning      bool
	probeExecuting    bool
	manualTakeover    bool   // process-local; ordinary resume must still revalidate
	trial             uint64 // admission generation for controlled real-task evidence
	nextSampleRetry   time.Time
	probeSettlement   *breakerProbeSettlement // fences the unlocked milestone callback phase
	sampleDispatching bool                    // owns the selected sample while SQL/requeue runs outside breakerMu
}

// SetBreakerProbe registers a semantic probe before Run. Empty stage is a site
// fallback. The context carries the existing request governor and site identity.
func (e *Engine) SetBreakerProbe(site, stage string, probe BreakerProbe) {
	e.breakers.SetBreakerProbe(site, stage, probe)
}

// InitBreakerRuntime restores protection before entry submission/recovery queues.
// Schema migration remains an explicit operation; an unreadable store stops startup.
func (e *Engine) InitBreakerRuntime(ctx context.Context) error {
	// Restoration itself uses shared storage. Register before touching it, and
	// keep this slot until both maintenance loops have joined the same lifetime.
	// The short admission lock also fences Stop; it is never held across I/O.
	e.mu.Lock()
	err := ctx.Err()
	if err == nil && e.ctx != nil {
		err = e.ctx.Err()
	}
	if err == nil {
		e.tickerWG.Add(1)
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	defer e.tickerWG.Done()
	restoreCtx, restoreCancel := context.WithCancel(ctx)
	defer restoreCancel()
	if e.ctx != nil {
		stopRestore := context.AfterFunc(e.ctx, restoreCancel)
		defer stopRestore()
	}
	releaseRestore, err := e.breakers.acquireRestore(restoreCtx)
	if err != nil {
		return err
	}
	defer releaseRestore()
	store, ready := e.breakers.prepareRestore(breakerstore.New(e.db))
	if ready {
		return nil
	}
	active, err := store.LoadRuntime(restoreCtx)
	if err != nil {
		return err
	}
	if err := restoreCtx.Err(); err != nil {
		return err
	}
	e.breakers.breakerMu.Lock()
	now := time.Now()
	for _, saved := range active {
		pending, protect, err := e.breakers.restoreSiteLocked(saved, now, e.breakerStageCoverage(saved.Control.Site))
		if err != nil {
			e.breakers.unlockAndEmit()
			return err
		}
		if e.breakers.hasSiteLocked(saved.Control.Site) {
			if g := e.Governance(); g != nil {
				g.RestoreRetryAfter(saved.Control.Site, saved.Control.RetryAfterUntil)
			}
		}
		if pending != nil {
			e.rememberUnsettled(pending)
		}
		if protect {
			e.pauseSmartBreakerLocked(saved.Control.Site, "恢复持久化熔断事件")
		}
	}

	e.breakers.unlockAndEmit()
	if err := e.breakers.flushBreakerReports(restoreCtx); err != nil {
		e.breakers.breakerMu.Lock()
		e.breakers.restorationFailedLocked()
		e.breakers.unlockAndEmit()
		return err
	}
	// An asynchronous restoration observer may stop the engine. The initializer
	// remains registered while checking cancellation and handing off to loops.
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	stop := func() bool { return false }
	if e.ctx != nil {
		stop = context.AfterFunc(e.ctx, cancel)
	}
	e.tickerWG.Add(2)
	go e.writeBreakerReports(loopCtx)
	go func() {
		defer e.tickerWG.Done()
		defer cancel()
		defer stop()
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case now := <-timer.C:
				e.tickBreakers(loopCtx, now)
				timer.Reset(200 * time.Millisecond)
			}
		}
	}()
	// Readiness includes the durable flush and maintenance handoff. Other
	// initializers remain behind the restore gate until this call returns.
	e.breakers.breakerMu.Lock()
	e.breakers.restorationPublishedLocked()
	e.breakers.unlockAndEmit()
	return nil
}

// Both healthy and failed writes wait on a timer. Coalescing has a fixed maximum
// wait instead of restarting a debounce timer for every terminal task.
func (e *Engine) writeBreakerReports(ctx context.Context) {
	defer e.tickerWG.Done()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := e.breakers.flushBreakerReports(writeCtx)
			if err == nil {
				e.settleBreakerFailures(writeCtx)
				e.retryBreakerSamples(writeCtx)
			}
			cancel()
			delay := 200 * time.Millisecond
			if err != nil {
				delay = time.Second
				if e.loggerSet != nil {
					e.loggerSet.Engine.Errorf("persist breaker report: %v", err)
				}
			}
			timer.Reset(delay)
		}
	}
}

func (e *Engine) pauseSmartBreakerLocked(site, reason string) bool {
	scope, state := e.breakers.protectionLocked(site, reason)
	return e.setPause(scope, state)
}

// Only this incident's automatic pauses can be released. Manual pauses and
// other owners survive, including manual stage pauses beneath an automatic site pause.
func (e *Engine) releaseSmartPauseLocked(site string) {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	changed := false
	for scope, st := range e.paused {
		if st.Source == PauseSourceBreaker && st.Owner == e.breakers.pauseOwnerLocked(site) {
			delete(e.paused, scope)
			e.recordResumeLocked(scope)
			changed = true
		}
	}
	if changed {
		e.broadcastPauseLocked()
		e.wakeDrainNow()
	}
}

func (e *Engine) BreakerReports(ctx context.Context, site string) ([]incident.Lifecycle, error) {
	return e.breakers.BreakerReports(ctx, site)
}

func (e *Engine) tickBreakers(ctx context.Context, now time.Time) {
	e.breakers.breakerMu.Lock()
	defer e.breakers.unlockAndEmit()
	if current := time.Now(); current.After(now) {
		now = current
	}
	for _, site := range e.breakers.siteKeysLocked() {
		reason, release, job := e.breakers.advanceSiteLocked(site, now, e.hasManualBreakerPause(site), e.breakerStageCoverage(site), e.breakerGovernanceEvidence(site))
		if release {
			e.releaseSmartPauseLocked(site)
		}
		if reason != "" {
			e.pauseSmartBreakerLocked(site, reason)
		}
		if job != nil {
			e.tickerWG.Add(1)
			go e.runBreakerProbe(ctx, job.probe, job.request, job.id, job.round, job.timeout)
		}
	}
}

func (e *Engine) runBreakerProbe(ctx context.Context, probe BreakerProbe, req BreakerProbeRequest, id string, round int, timeout time.Duration) {
	defer e.tickerWG.Done()
	probeCtx, cancel := context.WithTimeout(e.RuntimeContext(ctx, req.Site), timeout)
	defer cancel()
	type outcome struct {
		result BreakerProbeResult
		err    error
	}
	ch := make(chan outcome, 1)
	// An uncooperative extension cannot stall the manager. Track its goroutine so
	// Stop never reports fully drained while user probe code is still running.
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		defer e.breakers.probeExited(req.Site)

		var o outcome
		defer func() {
			if v := recover(); v != nil {
				o.err = fmt.Errorf("probe panic: %v", v)
			}
			ch <- o
		}()
		o.result, o.err = probe(probeCtx, req)
	}()
	var o outcome
	select {
	case o = <-ch:
	case <-probeCtx.Done():
		o.err = probeCtx.Err()
	}
	o.result.Detail = incident.Clean(incident.Sample{Error: o.result.Detail}).Error
	e.breakers.breakerMu.Lock()
	token, reason := e.breakers.settleStageProbeLocked(req, id, round, o.result, o.err, probeCtx)
	if token != nil {
		defer e.breakers.abandonStageProbeSettlement(token)
	}
	if reason != "" {
		e.pauseSmartBreakerLocked(req.Site, reason)
	}
	e.breakers.unlockAndEmit()
	if token == nil {
		return
	}
	e.breakers.breakerMu.Lock()
	if reason := e.breakers.finishStageProbeLocked(token, ctx); reason != "" {
		e.pauseSmartBreakerLocked(req.Site, reason)
	}
	e.breakers.unlockAndEmit()
}

func (e *Engine) breakerGovernanceEvidence(site string) breakerGovernanceEvidence {
	if g := e.Governance(); g != nil {
		p := g.Effective(site).Request
		return breakerGovernanceEvidence{available: true, autoThrottle: p.Enabled && p.AutoThrottle.Enabled, retryAfter: g.RetryAfterUntil(site)}
	}
	return breakerGovernanceEvidence{}
}

// Keep outcome mutation and pause publication in the same admission transaction.
func (e *Engine) recordSmartOutcome(task *Task, err error) (core.BreakerStatus, bool) {
	e.breakers.breakerMu.Lock()
	defer e.breakers.unlockAndEmit()
	reason, notify := e.breakers.recordSmartOutcomeLocked(task, err, e.breakerStageCoverage(task.Site), e.breakerGovernanceEvidence(task.Site))
	if reason == "" {
		return core.BreakerStatus{}, false
	}
	tripped := e.pauseSmartBreakerLocked(task.Site, reason)
	if !notify {
		return core.BreakerStatus{}, false
	}
	st := e.breakerStatusOfLocked(task.Site)
	if st.Stage == "" {
		st.Stage = task.Stage
	}
	return st, tripped
}
