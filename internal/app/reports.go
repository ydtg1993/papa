package app

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/analyze"
	"runtime/debug"
	"sort"
	"strconv"
	"time"
)

// UseReportSection 注册业务报告段（须在 Run 之前调用）。
//
// 报告引擎只认 obs.Section 接口；业务语义（业务表、业务事件、业务阈值）留在业务段里。
func (a *App) UseReportSection(sections ...obs.Section) {
	for _, s := range sections {
		if s == nil {
			panic(fmt.Errorf("UseReportSection: nil Section"))
		}
		a.reportSections = append(a.reportSections, s)
	}
}

// UseReportPattern 注册业务报告模式（须在 Run 之前调用）。
func (a *App) UseReportPattern(patterns ...obs.Pattern) {
	for _, p := range patterns {
		if p == nil {
			panic(fmt.Errorf("UseReportPattern: nil Pattern"))
		}
		a.reportPatterns = append(a.reportPatterns, p)
	}
}

// reports constructs the report component once; registration finishes before Run.
func (a *App) reports() *reportrunner.Runner {
	a.reportOnce.Do(func() {
		var logger reportrunner.Logger
		if a.Logger != nil {
			logger = a.Logger.Sys
		}
		a.reportRunner = reportrunner.New(reportrunner.Options{Config: a.reportConfig(), DB: a.DB, Logger: logger, Sections: a.reportSections, Patterns: a.reportPatterns, EngineState: a.obsEngineState, Inputs: a.obsReportInputs, Version: buildVersion()})
	})
	return a.reportRunner
}
func (a *App) generateExitReport()     { a.reports().Exit() }
func (a *App) startReportLoop()        { a.reports().Start() }
func (a *App) stopReportLoop()         { a.reports().Stop() }
func (a *App) generatePeriodicReport() { a.reports().Periodic() }

// obsEngineState 把引擎的只读快照转成 obs 的纯值上下文（obs 包不 import engine）。
//
// Engine 为 nil 时给 Source="none"：报告引擎据此知道「没有实时依据」，不做暂停/空闲跳过。
func (a *App) obsEngineState() obs.EngineState {
	st := obs.EngineState{Source: "none", CapturedAt: time.Now()}
	if a == nil || a.Engine == nil {
		return st
	}
	stats := a.Engine.GetSiteStats()
	sites := make([]obs.SiteState, 0, len(stats))
	for key, s := range stats {
		sites = append(sites, obs.SiteState{Key: key, Paused: s.BreakerPaused, PausedAt: s.BreakerPausedAt, StageCount: s.StageCount, PauseSource: s.PauseSource, PauseReason: s.PauseReason})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Key < sites[j].Key })
	st.Source = "engine"
	st.Sites = sites
	st.ActivePools = len(a.Engine.GetStageStats())
	return st
}

// buildVersion 返回当前主模块版本（开发构建为 "(devel)"）。
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// obsReportInputs 汇总报告自证信息里由宿主提供的部分：版本、配置路径与「生效阈值」。
//
// 阈值取配置归一后的值。测试/嵌入式构造的 Config 可能没走 config.Load，阈值是零值；
// 这里按 analyze 的默认口径补齐，避免报告把 0 当成生效值写出去。
// 时长统一换算成秒、用 FormatFloat 的最短表示：JSON 消费方拿到的是可直接解析的数字字符串。
func (a *App) obsReportInputs() obs.Inputs {
	stuckAfter := 10 * time.Minute
	failRateMax := 0.05
	eventMaxGap := 10 * time.Minute
	configFile := ""
	if a != nil {
		configFile = a.configPath
		if a.Config != nil {
			th := a.Config.Obs.Report.Thresholds
			if th.StuckAfter > 0 {
				stuckAfter = th.StuckAfter
			}
			if th.CrawlFailRateMax > 0 {
				failRateMax = th.CrawlFailRateMax
			}
			if th.EventMaxGap > 0 {
				eventMaxGap = th.EventMaxGap
			}
		}
	}
	return obs.Inputs{
		PapaVersion: buildVersion(),
		ConfigFile:  configFile,
		Thresholds: map[string]string{
			"stuck_after":         stuckAfter.String(),
			"crawl_fail_rate_max": strconv.FormatFloat(failRateMax, 'f', -1, 64),
			"event_max_gap":       eventMaxGap.String(),
		},
		ThresholdsSource: "effective（config 覆盖后，未校准）",
	}
}

// reportConfig detaches the runner from unrelated application configuration.
func (a *App) reportConfig() reportrunner.Config {
	if a.Config == nil {
		return reportrunner.Config{}
	}
	cfg := a.Config
	return reportrunner.Config{
		Enabled: cfg.Obs.Events.Enabled, LogDir: cfg.Log.Dir, EventsFile: cfg.Obs.Events.File,
		ReportDir: cfg.Obs.Report.Dir, OnExit: cfg.Obs.Report.OnExit,
		Interval: cfg.Obs.Report.Interval, Timeout: cfg.Obs.Report.Timeout,
		Retention: cfg.Obs.Report.Retention, Mode: cfg.Obs.Report.Mode,
		Thresholds: analyze.Thresholds{
			StuckAfter:       cfg.Obs.Report.Thresholds.StuckAfter,
			CrawlFailRateMax: cfg.Obs.Report.Thresholds.CrawlFailRateMax,
			EventMaxGap:      cfg.Obs.Report.Thresholds.EventMaxGap,
		},
	}
}
