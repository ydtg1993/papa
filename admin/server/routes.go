package server

import (
	"io/fs"
	"net/http"
)

// Register mounts the monitor page, feature APIs and composed page assets.
func (s *Monitor) Register(mux *http.ServeMux) {
	// 静态资源（CSS/JS），无需鉴权
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", noCache(http.FileServer(http.FS(sub)))))
	}

	mux.HandleFunc("/monitor", s.access.Wrap(s.page.ServeHTTP))
	mux.HandleFunc("/api/monitor", s.access.Wrap(s.monitoring.Snapshot))
	mux.HandleFunc("/api/task-flow", s.access.Wrap(s.taskflow.ServeHTTP))
	mux.HandleFunc("/api/settings", s.access.Wrap(s.settings.Settings))
	mux.HandleFunc("/api/settings/whitelist", s.access.Wrap(s.access.Whitelist))
	mux.HandleFunc("/api/settings/shutdown", s.access.Wrap(s.settings.Shutdown))
	mux.HandleFunc("/api/errorqueue/process", s.access.Wrap(s.queues.Error))
	mux.HandleFunc("/api/repeatqueue/process", s.access.Wrap(s.queues.Repeat))
	mux.HandleFunc("/api/breaker", s.access.Wrap(s.breakers.Status))
	mux.HandleFunc("/api/breaker/analysis", s.access.Wrap(s.breakers.Analysis))
	mux.HandleFunc("/api/breaker/resume", s.access.Wrap(s.breakers.Resume))
	mux.HandleFunc("/api/breaker/force-resume", s.access.Wrap(s.breakers.ForceResume))
	mux.HandleFunc("/api/breaker/pause", s.access.Wrap(s.breakers.Pause))
	mux.HandleFunc("/api/task/trace", s.access.Wrap(s.tasktrace.Trace))
	mux.HandleFunc("/api/logs", s.access.Wrap(s.logs.List))
	mux.HandleFunc("/api/logs/download", s.access.Wrap(s.logs.Download))
	mux.HandleFunc("/api/logs/tail", s.access.Wrap(s.logs.Tail))
	mux.HandleFunc("/api/task/page", s.access.Wrap(s.tasktrace.Page))
	// obs 报告页：列历史报告 + 读单份产物原文（download=1 下载）。目录来自 obs.report.dir。
	mux.HandleFunc("/api/obs/reports", s.access.Wrap(s.reports.List))
	mux.HandleFunc("/api/obs/report", s.access.Wrap(s.reports.File))
}
