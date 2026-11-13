package server

import (
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/breakers"
	"github.com/ydtg1993/papa/v4/admin/logs"
	"github.com/ydtg1993/papa/v4/admin/monitoring"
	"github.com/ydtg1993/papa/v4/admin/queues"
	"github.com/ydtg1993/papa/v4/admin/reports"
	"github.com/ydtg1993/papa/v4/admin/settings"
	"github.com/ydtg1993/papa/v4/admin/shell"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
	"github.com/ydtg1993/papa/v4/admin/tasktrace"
	"github.com/ydtg1993/papa/v4/core"
)

// Monitor 监控/后台管理 HTTP 路由(不负责 server 生命周期,统一由 App 层挂载)
type Monitor struct {
	page       *shell.Renderer
	access     *access.Handler
	settings   *settings.Handler
	monitoring *monitoring.Handler
	queues     *queues.Handler
	breakers   *breakers.Handler
	taskflow   *taskflow.Handler
	logs       *logs.Handler
	reports    *reports.Handler
	tasktrace  *tasktrace.Handler
}

// NewMonitor creates and connects the monitor feature handlers.
func NewMonitor(getter MonitorGetter, logger Logger, cfg MonitorConfig) *Monitor {
	s := &Monitor{}
	s.access = access.New(access.Options{VerifyToken: cfg.VerifyToken, AuthRequired: cfg.AuthRequired, SaveWhitelist: cfg.SaveWhitelist, Whitelist: cfg.Whitelist}, logger)
	s.page = shell.NewRenderer(templateFS, shell.RenderOptions{AuthRequired: s.access.AuthRequired, OptionalAssets: optionalOaoAssets, Logger: logger})
	s.settings = settings.New(settings.Options{Whitelist: s.access.WhitelistSnapshot, WhitelistPersistent: cfg.SaveWhitelist != nil, LogDir: cfg.LogDir, OnShutdown: cfg.OnShutdown, VerifyTokenValue: cfg.VerifyTokenValue}, logger)
	s.monitoring = monitoring.New(monitoring.Options{Stages: getter, QueueStats: cfg.QueueStats, BreakerStatuses: cfg.BreakerStatuses, SiteStats: cfg.SiteStats, SysInfo: cfg.SysInfo})
	s.queues = queues.New(queues.Options{ProcessErrorQueue: cfg.ProcessErrorQueue, ProcessRepeatQueue: cfg.ProcessRepeatQueue, OnQueueProcess: cfg.OnQueueProcess}, logger)
	s.breakers = breakers.New(breakers.Options{BreakerStatuses: cfg.BreakerStatuses, BreakerReports: cfg.BreakerReports, ResumeBreaker: cfg.ResumeBreaker, ForceResumeBreaker: cfg.ForceResumeBreaker, PauseBreaker: cfg.PauseBreaker, OnBreakerResume: cfg.OnBreakerResume, OnBreakerForceResume: cfg.OnBreakerForceResume, OnBreakerPause: cfg.OnBreakerPause}, logger)
	s.taskflow = taskflow.New(taskflow.Options{TaskFlow: cfg.TaskFlow, UpdateTaskFlow: cfg.UpdateTaskFlow, OnTaskFlowAction: cfg.OnTaskFlowAction})
	s.logs = logs.New(logs.Options{Dir: cfg.LogDir})
	s.reports = reports.New(reports.Options{Dir: cfg.ReportDir})
	s.tasktrace = tasktrace.New(tasktrace.Options{ArchiveDir: cfg.ArchiveDir, Trace: cfg.TaskTrace, Logger: logger})
	return s
}

// SetWhitelist publishes the host-provided source whitelist.
func (s *Monitor) SetWhitelist(entries []string) { s.access.SetWhitelist(entries) }

// SetTaskTraceExecution connects the optional execution-aware trace lookup.
// Call during composition before Register; MonitorConfig stays source-compatible.
func (s *Monitor) SetTaskTraceExecution(lookup func(int) ([]core.TraceExecutionStep, error)) {
	s.tasktrace.SetExecutionLookup(lookup)
}
