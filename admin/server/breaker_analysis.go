package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ydtg1993/papa/v3/obs/incident"
)

type breakerAnalysisView struct {
	incident.Report
	Text string `json:"text"`
}

func (s *Monitor) breakerAnalysisHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.BreakerIncidents == nil || s.cfg.BreakerStatuses == nil {
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
	for _, c := range s.cfg.BreakerIncidents(site) {
		if id != "" && c.ID != id {
			continue
		}
		report := incident.Analyze(c)
		views = append(views, breakerAnalysisView{Report: report, Text: incident.Text(report)})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if id != "" && len(views) == 0 {
		http.Error(w, "该熔断现场已过期或不存在", 404)
		return
	}
	if format != "" {
		if len(views) == 0 {
			http.Error(w, "本进程尚无熔断现场", 404)
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
		_ = json.NewEncoder(w).Encode(views[0].Report)
		return
	}
	writeJSON(w, map[string]any{"reports": views, "retention": "本进程每站最近 4 次，全局最多 32 次；恢复后保留。重启前的历史请在 obs 报告查看（需事件记录与报告生成已启用）。"})
}
