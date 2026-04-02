package server

import (
	"context"
	"embed"
	"encoding/json"
	"github.com/ydtg1993/papa/pkg/monitor"
	"html/template"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ydtg1993/papa/internal/crawler"
)

//go:embed template.html
var templateFS embed.FS

// MonitorGetter 定义获取所有阶段监控器的函数类型
type MonitorGetter func() map[string]*monitor.Monitor[*crawler.Task]

// Monitor Server监控 HTTP 服务器
type Monitor struct {
	port      int
	getter    MonitorGetter
	logger    Logger
	server    *http.Server
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

// NewMonitor 创建监控服务器
func NewMonitor(port int, getter MonitorGetter, logger Logger) *Monitor {
	return &Monitor{
		port:   port,
		getter: getter,
		logger: logger,
	}
}

// loadTemplate 加载 HTML 模板（懒加载，线程安全）
func (s *Monitor) loadTemplate() (*template.Template, error) {
	s.parseOnce.Do(func() {
		s.template, s.parseErr = template.ParseFS(templateFS, "template.html")
	})
	return s.template, s.parseErr
}

// Start 启动 HTTP 服务（非阻塞）
func (s *Monitor) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/monitor", s.htmlHandler)
	mux.HandleFunc("/api/monitor", s.apiHandler)

	addr := ":" + strconv.Itoa(s.port)
	s.server = &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			s.logger.Errorf("monitor server shutdown error: %v", err)
		}
	}()

	s.logger.Infof("monitor server starting on %s", addr)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Errorf("monitor server failed: %v", err)
	}
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
		s.logger.Errorf("load monitor template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if err := tmpl.Execute(w, nil); err != nil {
		s.logger.Errorf("execute monitor template: %v", err)
	}
}
