package server

import (
	"embed"
	"encoding/json"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/pkg/track"
	"html/template"
	"net/http"
	"sync"
)

//go:embed template.html
var templateFS embed.FS

// MonitorGetter 定义获取所有阶段监控器的函数类型
type MonitorGetter func() map[string]*track.StatsQueue[*crawler.Task]

// Monitor 监控 HTTP 路由(不负责 server 生命周期,统一由 App 层挂载)
type Monitor struct {
	getter    MonitorGetter
	logger    Logger
	template  *template.Template
	parseOnce sync.Once
	parseErr  error
}

// Logger 简单日志接口，避免循环依赖
type Logger interface {
	Info(args ...interface{})
	Infof(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// NewMonitor 创建监控路由
func NewMonitor(getter MonitorGetter, logger Logger) *Monitor {
	return &Monitor{
		getter: getter,
		logger: logger,
	}
}

// Register 将监控路由注册到统一 mux 上
func (s *Monitor) Register(mux *http.ServeMux) {
	mux.HandleFunc("/monitor", s.htmlHandler)
	mux.HandleFunc("/api/monitor", s.apiHandler)
}

// loadTemplate 加载 HTML 模板（懒加载，线程安全）
func (s *Monitor) loadTemplate() (*template.Template, error) {
	s.parseOnce.Do(func() {
		s.template, s.parseErr = template.ParseFS(templateFS, "template.html")
	})
	return s.template, s.parseErr
}

// apiHandler 返回 JSON 格式的监控数据
func (s *Monitor) apiHandler(w http.ResponseWriter, r *http.Request) {
	monitors := s.getter()
	if len(monitors) == 0 {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no monitors active"})
		return
	}

	data := make(map[string]interface{})
	for stage, mon := range monitors {
		globalStats := mon.GetGlobalStats()
		workerStats := mon.GetAllWorkerStats()
		data[stage] = map[string]interface{}{
			"global":  globalStats,
			"workers": workerStats,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

// htmlHandler 返回 HTML 监控页面（自动刷新）
func (s *Monitor) htmlHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := s.loadTemplate()
	if err != nil {
		s.logger.Errorf("load monitor template: %s", err.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if err := tmpl.Execute(w, nil); err != nil {
		s.logger.Errorf("execute monitor template: %s", err.Error())
	}
}
