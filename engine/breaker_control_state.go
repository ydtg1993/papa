package engine

import (
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"time"
)

// Coordinator operations own their state. Locked methods require breakerMu;
// callers publish cross-component pause changes before releasing that lock.

func (b *breakerCoordinator) SetSiteBreaker(site string, cfg breaker.Config) {
	if site == "" {
		return
	}
	cfg = breaker.EffectiveConfig(cfg)
	detector := breaker.NewDetector(cfg)
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if b.smartBreakers == nil {
		b.smartBreakers = make(map[string]*smartBreaker)
	}
	b.smartBreakers[site] = &smartBreaker{cfg: cfg, policy: cfg.Policy, detector: detector}
}

func (b *breakerCoordinator) breakerSum(site string) int {
	b.breakerMu.Lock()
	defer b.unlockAndEmit()
	if s := b.smartBreakers[site]; s != nil {
		return s.detector.Snapshot(time.Now()).Failures
	}
	return 0
}

func (b *breakerCoordinator) statusLocked(site string) BreakerStatus {
	var st BreakerStatus
	if s := b.smartBreakers[site]; s != nil {
		st.Enabled, st.Window, st.Threshold = s.cfg.Enabled, s.cfg.Window, s.cfg.Threshold
		st.InWindow = s.detector.Snapshot(time.Now()).Failures
		if s.event != nil {
			st.IncidentID = s.event.ID
			st.WindowFrom = s.event.report.Capture.From
		}
	}

	st.Site = site
	return st
}

func (b *breakerCoordinator) hasSiteLocked(site string) bool { return b.smartBreakers[site] != nil }

func (b *breakerCoordinator) takeoverLocked(scope pauseScope) string {
	s := b.smartBreakers[scope.Site]
	if s == nil || s.event == nil || !s.event.Active() || (scope.Stage != "" && s.event.Stage != scope.Stage) {
		return ""
	}
	s.manualTakeover = true
	owner := s.event.ID
	b.closeSmartBreakerLocked(scope.Site, scope.Stage, "人工接管自动保护；人工暂停仅在本进程有效")
	return owner
}

func (b *breakerCoordinator) manualPauseLocked(scope pauseScope, st pauseState) {
	if s := b.smartBreakers[scope.Site]; s != nil && s.event != nil {
		b.noteBreakerLocked(s, "manual_pause", scope.Stage, "人工暂停："+st.Reason+"；暂停仅本进程有效", st.At)
	}
}

func (b *breakerCoordinator) resumeDecisionLocked(site string, force bool) (blocked, retry, active bool) {
	s := b.smartBreakers[site]
	if s == nil || s.event == nil {
		return
	}
	if s.event.PendingFailure != nil {
		b.noteBreakerLocked(s, "failure_settlement_wait", "", "存在未结算失败，不能人工重试或强制放行；等待原轮次终态确认", time.Now())
		return true, false, false
	}
	active = s.event.Active()
	retry = !force && (active || s.manualTakeover)
	blocked = retry && (s.probeRunning || s.probeExecuting)
	return
}

func (b *breakerCoordinator) retrySiteLocked(site string, coverage map[string]string) bool {
	s := b.smartBreakers[site]
	s.manualTakeover = false
	return b.retryBreakerLocked(s, coverage)
}

func (b *breakerCoordinator) forceResumeLocked(site string) {
	if s := b.smartBreakers[site]; s != nil {
		s.manualTakeover = false
	}
	b.closeSmartBreakerLocked(site, "", "人工强制解除整站保护")
}

func (b *breakerCoordinator) resumeStageLocked(site, stage string) {
	if s := b.smartBreakers[site]; s != nil && s.event != nil {
		if s.event.Stage == stage {
			b.closeSmartBreakerLocked(site, stage, "人工恢复阶段")
		} else {
			b.noteBreakerLocked(s, "manual_resume", stage, "人工解除本阶段暂停；不证明整站健康，不清空计数", time.Now())
		}
	}
}

func (b *breakerCoordinator) siteKeysLocked() []string {
	out := make([]string, 0, len(b.smartBreakers))
	for site := range b.smartBreakers {
		out = append(out, site)
	}
	return out
}

// protectionLocked projects the automatic pause without lending out evidence.
func (b *breakerCoordinator) protectionLocked(site, reason string) (pauseScope, pauseState) {
	l := b.smartBreakers[site].event
	stage := l.Stage
	if stage == "" && len(l.report.Evidence) > 0 {
		stage = l.report.Evidence[len(l.report.Evidence)-1].Stage
	}
	return pauseScope{Site: l.Site, Stage: l.Stage}, pauseState{Reason: reason, Source: PauseSourceBreaker, Owner: l.ID, Stage: stage, Failures: l.report.Capture.Failures}
}
func (b *breakerCoordinator) pauseOwnerLocked(site string) string {
	return b.smartBreakers[site].event.ID
}
