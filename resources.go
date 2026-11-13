package papa

import (
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
)

// ReportInputLimits configures bounded report loading before App.Run.
type ReportInputLimits = inputbudget.Limits

// DownloadLimits bounds one download without changing the legacy downloader configuration.
type DownloadLimits = middleware.DownloadLimits

// DownloadBudget owns shared reservations across downloader instances.
type DownloadBudget = middleware.DownloadBudget

// NewDownloadBudget selects finite defaults for nonpositive values. Reservations
// bound managed buffers and temporary work; they are not a process RSS limit.
func NewDownloadBudget(memoryBytes, diskBytes int64) *DownloadBudget {
	return middleware.NewDownloadBudget(memoryBytes, diskBytes)
}

// SchedulerLimits bounds resident references across pools, spill and delay.
type SchedulerLimits = engine.SchedulerLimits
