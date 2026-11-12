package engine

import (
	"fmt"

	"github.com/ydtg1993/papa/v4/internal/breaker"
)

func recoverySampleID(sample breaker.RecoverySample) string {
	if sample.TaskID > 0 {
		return fmt.Sprintf("task:%d", sample.TaskID)
	}
	if sample.URL != "" {
		return "url:" + sample.URL
	}
	return ""
}

func (e *Engine) hasManualBreakerPause(site string) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	for scope, state := range e.paused {
		if scope.Site == site && state.Source == PauseSourceManual {
			return true
		}
	}
	return false
}

// breakerStageCoverage supplies immutable registration declarations, not Engine state.
func (e *Engine) breakerStageCoverage(site string) map[string]string {
	coverage := make(map[string]string)
	for _, stage := range e.stages {
		if stage.config.Site == site {
			coverage[stage.stage] = stage.config.BreakerReadOnlyCoverage
		}
	}
	return coverage
}
