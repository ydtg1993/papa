package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ydtg1993/papa/v4/obs/incident"
)

type breakerAnalysisView struct {
	incident.Report
	Text      string              `json:"text"`
	Lifecycle *incident.Lifecycle `json:"lifecycle,omitempty"`
}

func (s *Monitor) breakerAnalysisHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.BreakerReports == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "熔断分析未配置", 404)
		return
	}
	site := strings.TrimSpace(r.URL.Query().Get("scope"))
	if _, ok := s.cfg.BreakerStatuses()[site]; !ok || site == "" {
		http.Error(w, "需要有效站点 scope", 400)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "text" && format != "json" {
		http.Error(w, "未知导出格式", 400)
		return
	}
	id := r.URL.Query().Get("id")
	views := []breakerAnalysisView{}
	retention := "显示数据库中本站最近 100 次故障事件；事件内证据和时间线有上限，达到上限会标记。"
	reports, err := s.cfg.BreakerReports(r.Context(), site)
	if err != nil {
		http.Error(w, "持久化熔断报告读取失败，请检查服务日志和数据库", http.StatusServiceUnavailable)
		return
	}
	for _, report := range reports {
		if report.Capture.Site != site || (id != "" && report.Capture.ID != id) {
			continue
		}
		clean := report.Clean()
		views = append(views, breakerAnalysisView{Report: clean.Analysis(), Text: clean.Text(), Lifecycle: &clean})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if id != "" && len(views) == 0 {
		http.Error(w, "该熔断现场已过期或不存在", 404)
		return
	}
	if format != "" {
		if len(views) == 0 {
			http.Error(w, "尚无故障事件", 404)
			return
		}
		if format == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="breaker-analysis.txt"`)
			_, _ = w.Write([]byte(views[0].Text))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="breaker-analysis.json"`)
		_ = json.NewEncoder(w).Encode(views[0])
		return
	}
	writeJSON(w, map[string]any{"reports": views, "retention": retention})
}
