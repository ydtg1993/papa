package app

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"runtime/debug"
)

// reports constructs a signal-only component from static values, never DB or callbacks.
func (a *App) reports() *reportrunner.Runner {
	a.reportOnce.Do(func() {
		a.reportRunner = reportrunner.NewWithLimits(reportrunner.Options{Config: a.reportConfig(), RunID: a.runID, Inputs: a.obsReportInputs(), Version: buildVersion()}, a.reportInputLimits)
	})
	return a.reportRunner
}
func (a *App) startReportLoop() {
	r := a.reports()
	r.Start()
	if a.observation != nil {
		a.observation.report.Store(r)
	}
}

// buildVersion 返回当前主模块版本（开发构建为 "(devel)"）。
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// obsReportInputs contains only static provenance, not offline analysis knobs.
func (a *App) obsReportInputs() obs.Inputs {
	return obs.Inputs{PapaVersion: buildVersion(), ConfigFile: a.configPath, ThresholdsSource: "在线仅解释收到的信号；配置阈值仅用于离线分析"}
}

// reportConfig detaches the runner from unrelated application configuration.
func (a *App) reportConfig() reportrunner.Config {
	if a.Config == nil {
		return reportrunner.Config{}
	}
	cfg := a.Config
	return reportrunner.Config{
		Enabled:   cfg.Obs.Events.Enabled,
		ReportDir: cfg.Obs.Report.Dir, OnExit: cfg.Obs.Report.OnExit,
		Interval:  cfg.Obs.Report.Interval,
		Retention: cfg.Obs.Report.Retention,
	}
}

// UseReportInputLimits sets finite input budgets before Run constructs the report
// component. It is a composition operation, not a runtime policy update.
func (a *App) UseReportInputLimits(limits inputbudget.Limits) error {
	if a.reportRunner != nil {
		return fmt.Errorf("report input limits already initialized")
	}
	a.reportInputLimits = limits.Normalize()
	return nil
}
